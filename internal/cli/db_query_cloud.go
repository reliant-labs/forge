package cli

// The hosted transport of `forge db query`: a workload that runs OnHosted
// keeps its database inside the platform, and the control plane's
// AppDatabaseService.QueryDatabase runs the statement there, as the env's
// forge_ro role, with the same forge/pkg/dbquery core the other transports
// run locally (DESIGN §7.2). forge vendors none of the control plane's
// protos: the request and reply are declared here as protojson shapes.
//
// The control plane is the authority on everything a hosted query may do. It
// reads the env's data_access policy from the RECORDED release shape, not
// from this checkout's KCL, so forge sends what the person asked for
// (--confirm-env becomes confirm_env_id, --reason is the reason) and maps the
// answer onto the exit table; it does not pre-judge the policy.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/reliant-labs/forge/internal/cloud"
	"github.com/reliant-labs/forge/internal/deploytarget"
	"github.com/reliant-labs/forge/pkg/dbquery"
)

const procQueryDatabase = "controlplane.v1.AppDatabaseService/QueryDatabase"

// dbQueryCaller is what the hosted transport needs from a control-plane
// client. *cloud.Client satisfies it.
type dbQueryCaller interface {
	Call(ctx context.Context, procedure string, req, out any) error
}

// dbQueryCloudOpen resolves env's control plane and the control plane's id
// for the env. A variable so a test can substitute a fake control plane.
var dbQueryCloudOpen = func(ctx context.Context, env string) (dbQueryCaller, string, error) {
	ep, cred, err := resolveCloudTarget(ctx, env, "")
	if err != nil {
		return nil, "", exitCodeError{code: exitUndetermined, msg: fmt.Sprintf("env %s: cannot reach its control plane: %v", env, err)}
	}
	client := cloud.NewClient(ep, cred)
	id, err := deploytarget.LookupHostedEnvironment(ctx, client, hostedProjectName(), env)
	if errors.Is(err, deploytarget.ErrHostedEnvironmentNotFound) {
		return nil, "", exitCodeError{code: exitUndetermined, msg: fmt.Sprintf("env %s does not exist on its control plane yet: nothing has been deployed to it (%s env deploy %s)", env, Name(), env)}
	}
	if err != nil {
		return nil, "", dbQueryCloudError(err)
	}
	return client, id, nil
}

// dbQueryAudit is what the control plane recorded about a hosted query.
type dbQueryAudit struct {
	// ID is the audit_log row of the call.
	ID string `json:"id,omitempty"`
	// SQLSHA256 is the statement's hash, the form the activity feed carries.
	SQLSHA256 string `json:"sql_sha256,omitempty"`
	// Replica says which server answered: "ro" (a read replica) or "rw".
	Replica string `json:"replica,omitempty"`
}

// dbQueryRemote runs a statement somewhere forge does not connect to itself,
// and returns the result with what the remote side recorded and the
// database that answered.
type dbQueryRemote func(ctx context.Context, sql string) (*dbquery.Result, *dbQueryAudit, string, error)

// resolveDBQueryHosted is the target of a hosted workload: no DSN, a remote
// run through the control plane.
func resolveDBQueryHosted(o dbQueryOptions, scope dbQueryScope) dbQueryTarget {
	scope.Transport = "control-plane"
	scope.Database = o.database
	return dbQueryTarget{scope: scope, remote: func(ctx context.Context, sql string) (*dbquery.Result, *dbQueryAudit, string, error) {
		caller, envID, err := dbQueryCloudOpen(ctx, o.env)
		if err != nil {
			return nil, nil, "", err
		}
		return queryDatabaseHosted(ctx, caller, envID, o, sql)
	}}
}

type wireQueryDatabase struct {
	Columns []struct {
		Name   string `json:"name"`
		PGType string `json:"pgType"`
	} `json:"columns"`
	Rows []struct {
		Values []json.RawMessage `json:"values"`
	} `json:"rows"`
	Truncated  bool      `json:"truncated"`
	DurationMS wireInt64 `json:"durationMs"`
	Replica    string    `json:"replica"`
	AuditID    string    `json:"auditId"`
	SQLSHA256  string    `json:"sqlSha256"`
	Database   string    `json:"database"`
}

// queryDatabaseHosted calls QueryDatabase for envID and converts the reply
// into dbquery's result, so every output format renders it as it renders a
// local query.
func queryDatabaseHosted(ctx context.Context, caller dbQueryCaller, envID string, o dbQueryOptions, sql string) (*dbquery.Result, *dbQueryAudit, string, error) {
	body := map[string]any{
		"environmentId": envID,
		"sql":           sql,
		"maxRows":       o.maxRows,
		"timeoutMs":     o.timeout.Milliseconds(),
	}
	if o.database != "" {
		body["database"] = o.database
	}
	// --confirm-env names the env as the person knows it; the control plane
	// compares ids. A confirmation of a different env is not sent, so an
	// audited env refuses with confirmation_required, exactly as locally.
	if o.confirmEnv != "" && o.confirmEnv == o.env {
		body["confirmEnvId"] = envID
	}
	if r := strings.TrimSpace(o.reason); r != "" {
		body["reason"] = r
	}
	var w wireQueryDatabase
	if err := caller.Call(ctx, procQueryDatabase, body, &w); err != nil {
		return nil, nil, "", dbQueryCloudError(err)
	}
	res, err := resultOfWire(w)
	if err != nil {
		return nil, nil, "", exitCodeError{code: exitUndetermined, msg: fmt.Sprintf("the control plane's answer is unreadable: %v", err)}
	}
	return res, &dbQueryAudit{ID: w.AuditID, SQLSHA256: w.SQLSHA256, Replica: w.Replica}, w.Database, nil
}

func resultOfWire(w wireQueryDatabase) (*dbquery.Result, error) {
	res := &dbquery.Result{Columns: make([]dbquery.Column, len(w.Columns)), Rows: make([][]any, 0, len(w.Rows)), Truncated: w.Truncated}
	for i, c := range w.Columns {
		res.Columns[i] = dbquery.Column{Name: c.Name, PGType: c.PGType}
	}
	for _, r := range w.Rows {
		row := make([]any, len(r.Values))
		for i, raw := range r.Values {
			v, err := cellOfWire(raw)
			if err != nil {
				return nil, err
			}
			row[i] = v
			res.Bytes += len(bytes.TrimSpace(raw))
		}
		res.Rows = append(res.Rows, row)
	}
	res.RowCount = len(res.Rows)
	if res.Truncated {
		// The control plane does not say which cap cut the result. With rows
		// kept it is the row cap, the one a caller can raise; with none, the
		// response-size cap.
		res.TruncatedBy = "rows"
		if res.RowCount == 0 {
			res.TruncatedBy = "bytes"
		}
	}
	res.DurationMS = w.DurationMS.Int64()
	return res, nil
}

// cellOfWire turns one google.protobuf.Value into the cell types dbquery
// produces: nil, bool, json.Number, string, and json.RawMessage for an
// object or a list (a json/jsonb column).
func cellOfWire(raw json.RawMessage) (any, error) {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 {
		return nil, nil
	}
	switch raw[0] {
	case '{', '[':
		var buf bytes.Buffer
		if err := json.Compact(&buf, raw); err != nil {
			return nil, err
		}
		return json.RawMessage(buf.Bytes()), nil
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil, err
	}
	return v, nil
}

// dbQueryCloudFailure is a hosted query the control plane refused or could
// not answer, with its exit code. It keeps the *cloud.Error reachable, so the
// JSON document's reason is the control plane's: dbquery's subcode, or a
// policy reason such as confirmation_required.
type dbQueryCloudFailure struct {
	code int
	msg  string
	err  error
}

func (e dbQueryCloudFailure) Error() string { return e.msg }
func (e dbQueryCloudFailure) Unwrap() error { return e.err }
func (e dbQueryCloudFailure) ExitCode() int { return e.code }

// dbQueryWrongPreconditions are the FailedPrecondition reasons that mean the
// request, not the platform, must change.
var dbQueryWrongPreconditions = map[string]bool{"confirmation_required": true, "no_database": true}

// dbQueryCloudError maps a control-plane failure onto the exit table the
// local transports use: 1 when the request is wrong (the guard or the
// read-only transaction refused it, a SQL or permission error, the policy
// refused it, a missing confirmation, no such database); 2 when forge could
// not look (unreachable, signed out, rate limited, timed out, the env's
// read-only role not provisioned yet, a control plane that does not serve the
// call).
func dbQueryCloudError(err error) error {
	var ce *cloud.Error
	if !errors.As(err, &ce) {
		return dbQueryCloudFailure{code: exitUndetermined, msg: fmt.Sprintf("the control plane could not be reached: %v", err), err: err}
	}
	msg := ce.Message
	if msg == "" {
		msg = ce.Error()
	}
	code := exitUndetermined
	switch ce.Code {
	case cloud.CodeInvalidArgument, cloud.CodePermissionDenied, "not_found", "out_of_range", "already_exists":
		code = exitWrong
	case cloud.CodeFailedPrecondition:
		if dbQueryWrongPreconditions[ce.Reason] {
			code = exitWrong
		}
	case cloud.CodeUnimplemented:
		msg = "the control plane does not serve database queries yet (AppDatabaseService.QueryDatabase is unimplemented)"
	}
	return dbQueryCloudFailure{code: code, msg: msg, err: err}
}

// dbQueryErrorReason is the machine-readable reason of a failed query: the
// local executor's subcode, or the control plane's.
func dbQueryErrorReason(err error) string {
	var de *dbquery.Error
	if errors.As(err, &de) {
		return de.Reason
	}
	var ce *cloud.Error
	if errors.As(err, &ce) {
		return ce.Reason
	}
	return ""
}
