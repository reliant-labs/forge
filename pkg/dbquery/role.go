package dbquery

import (
	"context"
	"fmt"
	"sort"
	"time"

	"github.com/jackc/pgx/v5"
)

// Role is what the connected role could do beyond reading, as the server
// itself reports it.
//
// The read-only transaction [Run] opens refuses writes, but it is a property
// of the transaction, not of the role: the same role, in a session of its own
// (psql with the same password), may write, terminate other sessions with
// pg_terminate_backend, and read every row its privileges reach. A caller
// that holds a role's password for a real database should know which kind it
// holds, and [InspectRole] tells it without trusting a declaration.
type Role struct {
	// Name is current_user.
	Name string `json:"name"`
	// Writes lists what makes the role more than a reader, in a stable
	// order: role attributes (SUPERUSER, CREATEROLE, CREATEDB, REPLICATION,
	// BYPASSRLS), predefined roles it can act as (pg_signal_backend,
	// pg_write_all_data, ...), and relations it owns or may write. Empty
	// means a reader.
	Writes []string `json:"writes,omitempty"`
}

// Writable reports whether the role is more than a reader.
func (r Role) Writable() bool { return len(r.Writes) > 0 }

// powerRoles are the predefined roles whose members can change data or
// affect other sessions. A role that can act as one of them is not a reader,
// whatever its table grants say. Named, not OID-pinned: a server that lacks
// one (older majors) simply does not match it.
var powerRoles = []string{
	"pg_signal_backend",         // pg_terminate_backend / pg_cancel_backend on any non-superuser session
	"pg_write_all_data",         // INSERT/UPDATE/DELETE on every table
	"pg_write_server_files",     // COPY ... TO a server file
	"pg_execute_server_program", // COPY ... PROGRAM
	"pg_checkpoint",
	"pg_create_subscription",
}

// roleProbe asks the server what current_user can do, in one statement.
//
// MEMBER, not USAGE: a role that can SET ROLE to an owner can act as that
// owner even when its membership does not inherit, so a NOINHERIT member of
// the app role is still the app role. Ownership of a relation is counted
// separately from write grants because an owner can grant itself anything
// and ALTER or DROP the relation.
const roleProbe = `
WITH me AS (
  SELECT r.rolname, r.rolsuper, r.rolcreaterole, r.rolcreatedb, r.rolreplication, r.rolbypassrls
    FROM pg_catalog.pg_roles r WHERE r.rolname = current_user
), rels AS (
  SELECT c.oid, c.relowner
    FROM pg_catalog.pg_class c JOIN pg_catalog.pg_namespace n ON n.oid = c.relnamespace
   WHERE c.relkind IN ('r', 'p', 'v', 'm', 'f')
     AND n.nspname NOT IN ('pg_catalog', 'information_schema')
     AND n.nspname !~ '^pg_toast'
)
SELECT me.rolname, me.rolsuper, me.rolcreaterole, me.rolcreatedb, me.rolreplication, me.rolbypassrls,
       ARRAY(SELECT p.rolname::text FROM pg_catalog.pg_roles p
              WHERE p.rolname = ANY ($1::text[]) AND pg_catalog.pg_has_role(current_user, p.oid, 'MEMBER')
              ORDER BY 1),
       (SELECT count(*) FROM rels WHERE pg_catalog.pg_has_role(current_user, rels.relowner, 'MEMBER')),
       (SELECT count(*) FROM rels
         WHERE pg_catalog.has_table_privilege(rels.oid, 'INSERT, UPDATE, DELETE, TRUNCATE')
            OR pg_catalog.has_any_column_privilege(rels.oid, 'INSERT, UPDATE'))
  FROM me`

// InspectRole reports what the connected role could do beyond reading. It
// reads only the catalogs, in a READ ONLY transaction under a short statement
// timeout, and changes nothing. An error means the server could not be asked;
// a caller that needs a reader must treat it as "not known to be one".
func InspectRole(ctx context.Context, db Beginner) (Role, error) {
	ctx, cancel := context.WithTimeout(ctx, DefaultTimeout+ctxGrace)
	defer cancel()
	tx, err := db.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted, AccessMode: pgx.ReadOnly})
	if err != nil {
		return Role{}, classify(err)
	}
	defer func() {
		rctx, rcancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer rcancel()
		_ = tx.Rollback(rctx)
	}()
	if _, err := tx.Exec(ctx, `SELECT pg_catalog.set_config('statement_timeout', $1, true)`,
		fmt.Sprintf("%dms", DefaultTimeout.Milliseconds())); err != nil {
		return Role{}, classify(err)
	}
	var (
		r                                         Role
		super, createRole, createDB, repl, bypass bool
		power                                     []string
		owned, writable                           int64
	)
	if err := tx.QueryRow(ctx, roleProbe, powerRoles).Scan(
		&r.Name, &super, &createRole, &createDB, &repl, &bypass, &power, &owned, &writable,
	); err != nil {
		return Role{}, classify(err)
	}
	for _, a := range []struct {
		on   bool
		name string
	}{{super, "SUPERUSER"}, {createRole, "CREATEROLE"}, {createDB, "CREATEDB"}, {repl, "REPLICATION"}, {bypass, "BYPASSRLS"}} {
		if a.on {
			r.Writes = append(r.Writes, a.name)
		}
	}
	sort.Strings(power)
	for _, p := range power {
		r.Writes = append(r.Writes, "member of "+p)
	}
	if owned > 0 {
		r.Writes = append(r.Writes, fmt.Sprintf("owns %d relations", owned))
	}
	if writable > 0 {
		r.Writes = append(r.Writes, fmt.Sprintf("may write %d relations", writable))
	}
	return r, nil
}
