// Package kclplugin registers forge's in-process KCL plugin namespace
// (kcl_plugin.forge.*), letting KCL pull host-runtime values during
// evaluation instead of forge having to pre-enumerate and inject them.
//
// Today it provides resolve_port — a pure-Go free-port allocator. KCL
// declares `port = forge.resolve_port("reliant-web", 3000)` inline, by
// any name it likes, and binds it to a variable other declarations
// reference (the frontend's port, env-var URLs, CORS origins). One
// declaration, referenced everywhere — forge owns the allocation, KCL
// owns the plumbing.
//
// The plugin bridge that lets KCL call back into Go is CGO-only (see
// register_cgo.go / register_nocgo.go). forge's distributed binaries are
// built with CGO so the namespace is always available; a CGO-free build
// still compiles (the render path is CGO-free via purego) but Register is
// a no-op, so KCL importing kcl_plugin.forge would fail to render there.
package kclplugin

import (
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"sync"
)

// PortResolver hands out host ports, one per logical name, stable for the
// life of the resolver: repeated Resolve("x", …) calls — including across
// the several KCL renders a single forge command performs — return the
// same port, so a component's bound port and the URLs composed from it
// never drift.
//
// Allocation order for a never-seen name:
//  1. the port it got on a previous run (when a store is configured and
//     that port is still free) — stable dev ports across runs;
//  2. the requested `preferred` port, then a short scan upward from it
//     (3000 → 3001 → …) so dev ports stay human-friendly and parity-stable;
//  3. an OS-assigned free port as the last resort.
//
// It never hands the same port to two names. Like any probe-then-bind
// scheme (cf. cloud-dev.sh's _pick_port), there is an inherent TOCTOU
// window between resolving a port and the launched process binding it;
// acceptable for the dev loop this serves.
type PortResolver struct {
	mu        sync.Mutex
	byName    map[string]int // confirmed this run
	claimed   map[int]bool
	persisted map[string]int // last run's assignments (tentative; reused if still free)
	storePath string         // when set, byName is saved here for cross-run reuse
}

// scanWindow bounds the upward search from the preferred port before
// falling back to an OS-assigned port.
const scanWindow = 64

func NewPortResolver() *PortResolver {
	return &PortResolver{byName: map[string]int{}, claimed: map[int]bool{}, persisted: map[string]int{}}
}

// NewPersistentPortResolver remembers assignments in a JSON file at path,
// so a name reuses the same port across forge runs. Best-effort: a
// read/write error never fails a render — it degrades to fresh allocation.
func NewPersistentPortResolver(path string) *PortResolver {
	r := NewPortResolver()
	r.storePath = path
	if data, err := os.ReadFile(path); err == nil {
		_ = json.Unmarshal(data, &r.persisted)
	}
	return r
}

// Resolve returns the port for name (see allocation order on PortResolver).
func (r *PortResolver) Resolve(name string, preferred int) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	if p, ok := r.byName[name]; ok {
		return p, nil
	}
	// 1. Reuse last run's port for this name when still available.
	if p, ok := r.persisted[name]; ok && p > 0 && !r.claimed[p] && portFree(p) {
		return r.assign(name, p), nil
	}
	// 2. Prefer the requested port, then scan upward from it.
	if preferred > 0 {
		for off := 0; off < scanWindow; off++ {
			cand := preferred + off
			if cand > 65535 {
				break
			}
			if !r.claimed[cand] && portFree(cand) {
				return r.assign(name, cand), nil
			}
		}
	}
	// 3. OS-assigned free port.
	for i := 0; i < 100; i++ {
		p, err := freePort()
		if err != nil {
			return 0, err
		}
		if !r.claimed[p] {
			return r.assign(name, p), nil
		}
	}
	return 0, fmt.Errorf("resolve_port(%q): exhausted free-port search", name)
}

// assign records the allocation and persists the confirmed map (best-effort).
func (r *PortResolver) assign(name string, port int) int {
	r.byName[name] = port
	r.claimed[port] = true
	if r.storePath != "" {
		if data, err := json.MarshalIndent(r.byName, "", "  "); err == nil {
			_ = os.MkdirAll(filepath.Dir(r.storePath), 0o755)
			_ = os.WriteFile(r.storePath, data, 0o644)
		}
	}
	return port
}

func portFree(p int) bool {
	ln, err := net.Listen("tcp", fmt.Sprintf("localhost:%d", p))
	if err != nil {
		return false
	}
	_ = ln.Close()
	return true
}

func freePort() (int, error) {
	ln, err := net.Listen("tcp", "localhost:0")
	if err != nil {
		return 0, err
	}
	defer ln.Close()
	return ln.Addr().(*net.TCPAddr).Port, nil
}

// defaultResolver backs the kcl_plugin.forge.resolve_port method. Process-
// global so ports stay stable across the renders one forge command runs.
var defaultResolver = NewPortResolver()

// UsePortStore swaps the global resolver for one that persists assignments
// to path (cross-run port stability). Call once before rendering, only on
// the dev-launch path — not for read-only renders like `forge ci`, which
// shouldn't write a ports file. Safe to call repeatedly.
func UsePortStore(path string) { defaultResolver = NewPersistentPortResolver(path) }

// ResetForTest clears the default resolver's allocations. Test-only.
func ResetForTest() { defaultResolver = NewPortResolver() }
