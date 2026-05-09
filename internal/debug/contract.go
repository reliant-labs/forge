// Package debug wraps a Delve debugger so forge can drive a debugging
// session from the CLI / MCP layers.
//
// The package exposes two complementary surfaces:
//
//   - [Debugger] — the rich per-session control surface (breakpoints,
//     execution control, inspection, lifecycle). One implementation today
//     (Delve via rpc2). Constructed via [NewDelveDebugger]; callers hold
//     the handle for the duration of a session.
//
//   - [Service] — the package's behavioral seam: load, save, and clear
//     the persisted SessionInfo on disk. Tests intercept the on-disk
//     session file by injecting a mock Service.
//
// The data carriers (SessionInfo, StopState, Variable, BreakpointInfo,
// StackFrame, GoroutineInfo) remain plain types — they are not behavior
// to mock.
//
// NewDebugger lives on Service so tests can inject a mock Debugger.
// Forge's mock generator emits "return nil" for interface-typed
// returns (both local interfaces like Debugger and well-known
// cross-package interfaces like io.Reader).
package debug

// Service is the behavioral surface of the debug package.
//
// Session helpers (Load/Save/Clear) hang off this interface so tests can
// intercept the on-disk session file without touching the filesystem.
type Service interface {
	// LoadSession reads .forge/debug-session.json from dir. Returns
	// (nil, nil) when no session file exists.
	LoadSession(dir string) (*SessionInfo, error)

	// SaveSession writes session to dir/.forge/debug-session.json.
	SaveSession(dir string, session *SessionInfo) error

	// ClearSession removes the session file from dir.
	ClearSession(dir string) error

	// NewDebugger constructs a Debugger handle for a fresh debugging
	// session. Returns an interface-typed value — the mock generator
	// must emit "return nil" here, not "return Debugger{}".
	NewDebugger() Debugger
}

// Deps is the dependency set for the debug Service. Empty today — Delve
// is invoked via process exec / rpc2 and needs no injected collaborators.
type Deps struct{}

// New constructs a debug.Service.
func New(_ Deps) Service { return &svc{} }

type svc struct{}

// LoadSession delegates to the package-level loader.
func (s *svc) LoadSession(dir string) (*SessionInfo, error) { return loadSession(dir) }

// SaveSession delegates to the package-level writer.
func (s *svc) SaveSession(dir string, session *SessionInfo) error {
	return saveSession(dir, session)
}

// ClearSession delegates to the package-level deleter.
func (s *svc) ClearSession(dir string) error { return clearSession(dir) }

// NewDebugger delegates to the Delve-backed constructor.
func (s *svc) NewDebugger() Debugger { return NewDelveDebugger() }
