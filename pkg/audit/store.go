// Package audit provides the durable write-side of forge's audit trail: an
// [Entry] record, a [Store] persistence interface, and a Postgres-backed
// [DBAuditStore] that writes and queries the audit_log table.
//
// This is the mechanical, reusable half — the part that is the same in every
// project and get-it-wrong-and-you-lose-your-compliance-record. It used to
// ship as the audit-log PACK (copied into each project); it is now a versioned
// library you IMPORT, so a fix here reaches every project on the next bump.
//
// Everything APP-SPECIFIC about audit is code you own, not library code:
//
//   - the audit_log TABLE — add it with the normal entity flow
//     (`forge scaffold entity`), owning the migration; the columns this
//     store reads/writes are documented on [Entry].
//   - the READ-SIDE ListAuditEvents RPC + its Connect handler — own the proto
//     and the handler, backing it with this [Store].
//
// The observability skill's "Audit log (recipe)" shows the full wiring; the
// short version is:
//
//	// write side — one line in the interceptor chain (see audit.Interceptor):
//	store := audit.NewDBAuditStore(db)
//	// observe.Chain(observe.Deps{Audit: audit.Interceptor(logger, claimsFrom, store)})
//
//	// read side — your owned handler delegates to the same store:
//	entries, err := store.Query(ctx, audit.Filter{UserID: "u_123", Limit: 50})
//
// The interceptor that produces entries lives alongside this store in
// [Interceptor] / [Sink]; it fans each RPC out to a [Store] off the request
// path so audit writes never add latency to a response.
package audit

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"time"
)

// Entry is a single audit-log record. Its fields mirror the columns of the
// audit_log table the read-side recipe creates: an audit write is one row per
// RPC capturing who (UserID/Email) did what (Procedure), from where
// (PeerAddress), how long it took (DurationMs), and how it ended
// (Status/ErrorCode/ErrorMessage). ID and CreatedAt are assigned by the
// database on insert and are only populated on rows read back via [Store.Query].
type Entry struct {
	ID           string            `json:"id" db:"id"`
	Timestamp    time.Time         `json:"timestamp" db:"timestamp"`
	UserID       string            `json:"user_id" db:"user_id"`
	Email        string            `json:"email" db:"email"`
	Procedure    string            `json:"procedure" db:"procedure"`
	PeerAddress  string            `json:"peer_address" db:"peer_address"`
	DurationMs   int               `json:"duration_ms" db:"duration_ms"`
	Status       string            `json:"status" db:"status"`
	ErrorCode    string            `json:"error_code,omitempty" db:"error_code"`
	ErrorMessage string            `json:"error_message,omitempty" db:"error_message"`
	Metadata     map[string]string `json:"metadata,omitempty" db:"metadata"`
	CreatedAt    time.Time         `json:"created_at" db:"created_at"`
}

// Filter is the query for [Store.Query]. All set fields are AND-combined; a
// zero-valued field is not constrained. Since/Until bound the Timestamp range
// (inclusive). A Limit <= 0 is treated as the default page size (100).
type Filter struct {
	UserID    string
	Procedure string
	Since     time.Time
	Until     time.Time
	Limit     int
	Offset    int
}

// Store is the audit-log persistence seam: [Interceptor] writes through it and
// the read-side ListAuditEvents handler reads through it. Implement it over
// any backend; [DBAuditStore] is the batteries-included *sql.DB implementation.
type Store interface {
	// Log persists a single audit entry.
	Log(ctx context.Context, entry Entry) error
	// Query returns audit entries matching filter, newest first.
	Query(ctx context.Context, filter Filter) ([]Entry, error)
}

// DBAuditStore is the *sql.DB-backed [Store] for the audit_log table. Construct
// it with [NewDBAuditStore]. It uses positional ($1, $2, …) placeholders and
// standard SQL, so it works against Postgres (the scaffold default) unchanged.
type DBAuditStore struct {
	db *sql.DB
}

// NewDBAuditStore returns a [DBAuditStore] backed by db. Pass the same store to
// [Interceptor] (write side) and the ListAuditEvents handler (read side) so
// reads observe the writes.
func NewDBAuditStore(db *sql.DB) *DBAuditStore {
	return &DBAuditStore{db: db}
}

// Log inserts entry into the audit_log table. ID and CreatedAt are left to the
// table's defaults (e.g. gen_random_uuid() / CURRENT_TIMESTAMP). A nil or
// unmarshalable Metadata is stored as an empty JSON object rather than failing
// the write — losing an audit row over a metadata quirk is the worse outcome.
func (s *DBAuditStore) Log(ctx context.Context, entry Entry) error {
	metadataJSON, err := json.Marshal(entry.Metadata)
	if err != nil {
		metadataJSON = []byte("{}")
	}

	_, err = s.db.ExecContext(ctx,
		`INSERT INTO audit_log (timestamp, user_id, email, procedure, peer_address, duration_ms, status, error_code, error_message, metadata)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)`,
		entry.Timestamp,
		entry.UserID,
		entry.Email,
		entry.Procedure,
		entry.PeerAddress,
		entry.DurationMs,
		entry.Status,
		nullString(entry.ErrorCode),
		nullString(entry.ErrorMessage),
		metadataJSON,
	)
	if err != nil {
		return fmt.Errorf("audit store: insert: %w", err)
	}
	return nil
}

// Query returns audit-log entries matching filter, ordered by timestamp
// descending. Filters are AND-combined; a nil filter (zero value) returns the
// most recent page. Pagination is offset-based with a default page size of 100.
func (s *DBAuditStore) Query(ctx context.Context, filter Filter) ([]Entry, error) {
	query := `SELECT id, timestamp, user_id, email, procedure, peer_address, duration_ms, status, error_code, error_message, metadata, created_at FROM audit_log WHERE 1=1`
	args := []any{}
	argIdx := 1

	if filter.UserID != "" {
		query += fmt.Sprintf(" AND user_id = $%d", argIdx)
		args = append(args, filter.UserID)
		argIdx++
	}
	if filter.Procedure != "" {
		query += fmt.Sprintf(" AND procedure = $%d", argIdx)
		args = append(args, filter.Procedure)
		argIdx++
	}
	if !filter.Since.IsZero() {
		query += fmt.Sprintf(" AND timestamp >= $%d", argIdx)
		args = append(args, filter.Since)
		argIdx++
	}
	if !filter.Until.IsZero() {
		query += fmt.Sprintf(" AND timestamp <= $%d", argIdx)
		args = append(args, filter.Until)
		argIdx++
	}

	query += " ORDER BY timestamp DESC"

	limit := filter.Limit
	if limit <= 0 {
		limit = 100
	}
	query += fmt.Sprintf(" LIMIT $%d", argIdx)
	args = append(args, limit)
	argIdx++

	if filter.Offset > 0 {
		query += fmt.Sprintf(" OFFSET $%d", argIdx)
		args = append(args, filter.Offset)
	}

	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("audit store: query: %w", err)
	}
	defer rows.Close()

	var entries []Entry
	for rows.Next() {
		var e Entry
		var errorCode, errorMessage sql.NullString
		var metadataJSON []byte

		if err := rows.Scan(
			&e.ID, &e.Timestamp, &e.UserID, &e.Email, &e.Procedure,
			&e.PeerAddress, &e.DurationMs, &e.Status,
			&errorCode, &errorMessage, &metadataJSON, &e.CreatedAt,
		); err != nil {
			return nil, fmt.Errorf("audit store: scan: %w", err)
		}

		e.ErrorCode = errorCode.String
		e.ErrorMessage = errorMessage.String

		if len(metadataJSON) > 0 {
			_ = json.Unmarshal(metadataJSON, &e.Metadata)
		}

		entries = append(entries, e)
	}
	return entries, rows.Err()
}

// nullString maps an empty string to a NULL column value so empty error
// fields are stored as NULL rather than "".
func nullString(s string) sql.NullString {
	if s == "" {
		return sql.NullString{}
	}
	return sql.NullString{String: s, Valid: true}
}
