//go:build ignore

package middleware

import (
	"context"
	"log/slog"
	"time"
)

// Standard attribute keys — use these consistently across all log events.
const (
	AttrProcedure  = "procedure"
	AttrRequestID  = "request_id"
	AttrTraceID    = "trace_id"
	AttrDuration   = "duration"
	AttrDurationMs = "duration_ms"
	AttrUserID     = "user_id"
	AttrEmail      = "email"
	AttrPeerAddr   = "peer"
	AttrStatus     = "status"
	AttrErrorCode  = "code"
	AttrComponent  = "component"
	AttrLogType    = "log_type"
	AttrService    = "service"
	AttrMethod     = "method"
)

// --- Request lifecycle events ---

// LogRequestCompleted logs a successful RPC completion at INFO level.
func LogRequestCompleted(ctx context.Context, logger *slog.Logger, attrs ...slog.Attr) {
	logger.LogAttrs(ctx, slog.LevelInfo, "rpc.ok", attrs...)
}

// LogRequestFailed logs a failed RPC at ERROR level.
func LogRequestFailed(ctx context.Context, logger *slog.Logger, attrs ...slog.Attr) {
	logger.LogAttrs(ctx, slog.LevelError, "rpc.error", attrs...)
}

// LogStreamCompleted logs a successful streaming RPC completion at INFO level.
func LogStreamCompleted(ctx context.Context, logger *slog.Logger, attrs ...slog.Attr) {
	logger.LogAttrs(ctx, slog.LevelInfo, "stream.ok", attrs...)
}

// LogStreamFailed logs a failed streaming RPC at ERROR level.
func LogStreamFailed(ctx context.Context, logger *slog.Logger, attrs ...slog.Attr) {
	logger.LogAttrs(ctx, slog.LevelError, "stream.error", attrs...)
}

// --- Server lifecycle events ---

// LogServerListening logs the server listening event at INFO level.
func LogServerListening(ctx context.Context, logger *slog.Logger, attrs ...slog.Attr) {
	logger.LogAttrs(ctx, slog.LevelInfo, "server.listening", attrs...)
}

// LogServerStopping logs the server shutdown event at INFO level.
func LogServerStopping(ctx context.Context, logger *slog.Logger, attrs ...slog.Attr) {
	logger.LogAttrs(ctx, slog.LevelInfo, "server.stopping", attrs...)
}

// --- Database events ---

// LogDBMigrationCompleted logs a successful auto-migration at INFO level.
func LogDBMigrationCompleted(ctx context.Context, logger *slog.Logger, attrs ...slog.Attr) {
	logger.LogAttrs(ctx, slog.LevelInfo, "db.migration.completed", attrs...)
}

// --- Worker events ---

// LogWorkerStarting logs a worker start event at INFO level.
func LogWorkerStarting(ctx context.Context, logger *slog.Logger, attrs ...slog.Attr) {
	logger.LogAttrs(ctx, slog.LevelInfo, "worker.starting", attrs...)
}

// LogWorkerError logs a worker error at ERROR level.
func LogWorkerError(ctx context.Context, logger *slog.Logger, attrs ...slog.Attr) {
	logger.LogAttrs(ctx, slog.LevelError, "worker.error", attrs...)
}

// --- Audit events ---

// LogAuditEvent logs an audit event at INFO level.
func LogAuditEvent(ctx context.Context, logger *slog.Logger, attrs ...slog.Attr) {
	logger.LogAttrs(ctx, slog.LevelInfo, "audit.event", attrs...)
}

// LogAuditEventWarn logs an audit event at WARN level (for failed operations).
func LogAuditEventWarn(ctx context.Context, logger *slog.Logger, attrs ...slog.Attr) {
	logger.LogAttrs(ctx, slog.LevelWarn, "audit.event", attrs...)
}

// --- Attribute helpers ---

// ProcedureAttr returns a standardized procedure attribute.
func ProcedureAttr(procedure string) slog.Attr {
	return slog.String(AttrProcedure, procedure)
}

// RequestIDAttr returns a standardized request ID attribute.
func RequestIDAttr(rid string) slog.Attr {
	return slog.String(AttrRequestID, rid)
}

// TraceIDAttr returns a standardized trace ID attribute.
func TraceIDAttr(traceID string) slog.Attr {
	return slog.String(AttrTraceID, traceID)
}

// DurationAttr returns a standardized duration attribute.
func DurationAttr(d time.Duration) slog.Attr {
	return slog.Duration(AttrDuration, d)
}

// DurationMsAttr returns a standardized duration attribute in milliseconds.
func DurationMsAttr(d time.Duration) slog.Attr {
	return slog.Int64(AttrDurationMs, d.Milliseconds())
}

// PeerAddrAttr returns a standardized peer address attribute.
func PeerAddrAttr(addr string) slog.Attr {
	return slog.String(AttrPeerAddr, addr)
}

// StatusAttr returns a standardized status attribute.
func StatusAttr(status string) slog.Attr {
	return slog.String(AttrStatus, status)
}

// ErrorCodeAttr returns a standardized error code attribute.
func ErrorCodeAttr(code string) slog.Attr {
	return slog.String(AttrErrorCode, code)
}

// UserIDAttr returns a standardized user ID attribute.
func UserIDAttr(userID string) slog.Attr {
	return slog.String(AttrUserID, userID)
}

// EmailAttr returns a standardized email attribute.
func EmailAttr(email string) slog.Attr {
	return slog.String(AttrEmail, email)
}
