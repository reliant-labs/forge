package dbgrants

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
)

// # The dashboard role
//
// [DashboardRoleName] is the role live dashboard panels query as. forge owns
// its LIFECYCLE and nothing else:
//
//   - The role exists in every database a forge migrator touches. The migrator
//     creates it NOLOGIN before any migration runs ([EnsureRole]), so a
//     migration may name it in dev, in tests, in CI and in production, whether
//     dashboards are on or not.
//   - The deploy decides whether it can log in. A ManagedDatabase renders it as
//     a CNPG managed role (pkg/deploy): LOGIN, a password Secret and a
//     connection limit while ManagedDatabase.dashboards is enabled, NOLOGIN
//     with no password while it is not. CNPG adopts a role that already
//     exists: its reconciler ALTERs a declared role it finds in the catalog to
//     the declared attributes instead of creating it.
//   - What it may READ is the project's: ordinary migrations grant it, column
//     by column (`GRANT SELECT (id, created_at, cost) ON messages TO
//     forge_dash;`), and a later migration revokes. forge renders no grants for
//     it and converges none.
//
// Switching dashboards off therefore leaves the grants in place on a role that
// cannot log in, so they are unusable until it can again.
//
// The one check forge keeps costs no annotation: a read-only role holds read
// privileges only. [DashboardPrivilegeViolations] lists anything else, and the
// migrator refuses to finish while the list is not empty (pkg/migratekit).

// ErrRoleNotCreatable means the role does not exist and the connecting role may
// not create it (it has neither CREATEROLE nor superuser). A ManagedDatabase's
// application role is such a role: there the role comes from CNPG's managed
// roles, which can reconcile after the first migration run starts.
var ErrRoleNotCreatable = errors.New("role does not exist and the connecting role may not create it")

// EnsureDashboardRole is [EnsureRole] for [DashboardRoleName].
func EnsureDashboardRole(ctx context.Context, c Conn) (created bool, err error) {
	return EnsureRole(ctx, c, DashboardRoleName)
}

// EnsureRole creates the role called name, NOLOGIN NOINHERIT with no password,
// when the cluster has none, and reports whether it did. An existing role is
// left exactly as it is: its LOGIN, password and connection limit belong to
// whoever declared it (CNPG), and so do its grants.
//
// Roles are cluster-wide, so two migrators of different databases on one
// server can race to create it; the loser finds it created and reports false.
// A connecting role that may not create roles gets an error wrapping
// [ErrRoleNotCreatable].
func EnsureRole(ctx context.Context, c Conn, name string) (created bool, err error) {
	if err := checkIdent(name); err != nil {
		return false, err
	}
	exists, err := RoleExists(ctx, c, name)
	if err != nil || exists {
		return false, err
	}
	// NOINHERIT matches the managed-role declaration (pkg/deploy), so CNPG
	// adopting it changes as little as possible.
	if _, err := c.ExecContext(ctx, "CREATE ROLE "+quoteIdent(name)+" NOLOGIN NOINHERIT"); err != nil {
		switch sqlState(err) {
		case "42710", "23505": // duplicate_object, unique_violation: another session created it first
			return false, nil
		case "42501": // insufficient_privilege
			return false, fmt.Errorf("dbgrants: %s: %w", name, ErrRoleNotCreatable)
		}
		return false, fmt.Errorf("dbgrants: create role %s: %w", name, err)
	}
	return true, nil
}

// sqlState is the SQLSTATE of a driver error, "" when it carries none. pgx's
// *pgconn.PgError and lib/pq's *pq.Error both implement SQLState().
func sqlState(err error) string {
	var s interface{ SQLState() string }
	if errors.As(err, &s) {
		return s.SQLState()
	}
	return ""
}

// DashboardPrivilegeViolations lists what role holds beyond reading, one
// sentence each, sorted: nil when it holds nothing else or does not exist.
//
// A read-only role needs SELECT (on tables, views, columns and sequences),
// USAGE on schemas and CONNECT on the database; everything else is listed:
//
//   - any other privilege on a database, schema, table, view, column,
//     sequence or large object, and any privilege held WITH GRANT OPTION;
//   - EXECUTE granted to the role itself (PUBLIC's default EXECUTE is not the
//     role's grant and is not listed; see [SecurityDefinerExposure]);
//   - ALTER DEFAULT PRIVILEGES entries naming the role, which would hand it
//     every table created later instead of the columns a migration named;
//   - membership in any role, and the SUPERUSER, CREATEROLE, CREATEDB,
//     REPLICATION and BYPASSRLS attributes.
//
// It reads the catalog of the database c is connected to (and the shared
// catalogs), so it covers the objects of that database only.
func DashboardPrivilegeViolations(ctx context.Context, c Conn, role string) ([]string, error) {
	var raw []byte
	if err := c.QueryRowContext(ctx, privilegeViolationsSQL, role).Scan(&raw); err != nil {
		return nil, fmt.Errorf("dbgrants: read the privileges of %s: %w", role, err)
	}
	var out []string
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("dbgrants: decode the privileges of %s: %w", role, err)
	}
	return out, nil
}

const privilegeViolationsSQL = `
WITH r AS (
  SELECT oid AS id, rolsuper, rolcreaterole, rolcreatedb, rolreplication, rolbypassrls FROM pg_roles WHERE rolname = $1
)
SELECT coalesce(json_agg(v.what ORDER BY v.what), '[]'::json) FROM (
  SELECT 'role attribute ' || a.name AS what
    FROM r, LATERAL (VALUES ('SUPERUSER', r.rolsuper), ('CREATEROLE', r.rolcreaterole), ('CREATEDB', r.rolcreatedb),
                            ('REPLICATION', r.rolreplication), ('BYPASSRLS', r.rolbypassrls)) AS a(name, held)
   WHERE a.held
  UNION ALL
  SELECT 'membership in role ' || quote_ident(pg_get_userbyid(m.roleid))
    FROM r JOIN pg_auth_members m ON m.member = r.id
  UNION ALL
  SELECT format('%s on database %I', x.privilege_type, d.datname) || CASE WHEN x.is_grantable THEN ' WITH GRANT OPTION' ELSE '' END
    FROM r, pg_database d, aclexplode(d.datacl) x
   WHERE d.datname = current_database() AND x.grantee = r.id AND (x.privilege_type <> 'CONNECT' OR x.is_grantable)
  UNION ALL
  SELECT format('%s on schema %I', x.privilege_type, n.nspname) || CASE WHEN x.is_grantable THEN ' WITH GRANT OPTION' ELSE '' END
    FROM r, pg_namespace n, aclexplode(n.nspacl) x
   WHERE x.grantee = r.id AND (x.privilege_type <> 'USAGE' OR x.is_grantable)
  UNION ALL
  SELECT format('%s on %s %s', x.privilege_type,
                CASE c.relkind WHEN 'S' THEN 'sequence' WHEN 'v' THEN 'view' WHEN 'm' THEN 'materialized view'
                               WHEN 'f' THEN 'foreign table' ELSE 'table' END,
                c.oid::regclass) || CASE WHEN x.is_grantable THEN ' WITH GRANT OPTION' ELSE '' END
    FROM r, pg_class c, aclexplode(c.relacl) x
   WHERE c.relkind IN ('r', 'p', 'v', 'm', 'f', 'S') AND x.grantee = r.id AND (x.privilege_type <> 'SELECT' OR x.is_grantable)
  UNION ALL
  SELECT format('%s on column %s.%I', x.privilege_type, c.oid::regclass, a.attname) || CASE WHEN x.is_grantable THEN ' WITH GRANT OPTION' ELSE '' END
    FROM r, pg_attribute a JOIN pg_class c ON c.oid = a.attrelid, aclexplode(a.attacl) x
   WHERE a.attnum > 0 AND NOT a.attisdropped AND x.grantee = r.id AND (x.privilege_type <> 'SELECT' OR x.is_grantable)
  UNION ALL
  SELECT format('%s on large object %s', x.privilege_type, l.oid) || CASE WHEN x.is_grantable THEN ' WITH GRANT OPTION' ELSE '' END
    FROM r, pg_largeobject_metadata l, aclexplode(l.lomacl) x
   WHERE x.grantee = r.id AND (x.privilege_type <> 'SELECT' OR x.is_grantable)
  UNION ALL
  SELECT format('EXECUTE on %s %s', CASE p.prokind WHEN 'p' THEN 'procedure' ELSE 'function' END, p.oid::regprocedure)
    FROM r, pg_proc p, aclexplode(p.proacl) x
   WHERE x.grantee = r.id
  UNION ALL
  SELECT format('default privileges of role %I%s on new %s', pg_get_userbyid(d.defaclrole),
                CASE WHEN d.defaclnamespace = 0 THEN '' ELSE format(' in schema %I', n.nspname) END,
                CASE d.defaclobjtype WHEN 'r' THEN 'tables' WHEN 'S' THEN 'sequences' WHEN 'f' THEN 'functions'
                                     WHEN 'T' THEN 'types' WHEN 'n' THEN 'schemas' WHEN 'L' THEN 'large objects' ELSE d.defaclobjtype::text END)
    FROM r, pg_default_acl d LEFT JOIN pg_namespace n ON n.oid = d.defaclnamespace, aclexplode(d.defaclacl) x
   WHERE x.grantee = r.id
) v`
