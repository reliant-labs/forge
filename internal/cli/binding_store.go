package cli

// The env→release binding ledger, behind a seam.
//
// WHY A SEAM AT ALL. Promotion state answers "which release does prod run",
// and today that answer lives in a file inside the repo that PRODUCED the
// artifact. That is circular by construction: the commit recording "prod runs
// v1.5.13" cannot be in v1.5.13, because it is written after v1.5.13 was cut.
// Promoting through N stages therefore costs N commits, each one a version
// bump that invalidates the artifact it describes. The way out is for the
// binding to live somewhere that is not the artifact's own source tree — which
// means a second backend, which means the callers must stop knowing that a
// binding is a file.
//
// THE FILE BACKEND IS THE DEFAULT, FOREVER. Not a stepping stone. forge with
// no account, on a plane, must stay fully functional: `forge env promote` and
// `forge env deploy` are core verbs, and a core verb that degrades without a
// login is a product that lied about being local-first. The precedent is buf —
// `buf lint`/`generate`/`breaking` work offline permanently, and BSR adds
// hosted distribution on top without ever becoming required. A solo user must
// never discover mid-task that they need to sign up.

// bindingStore is the env→release binding ledger as its CONSUMERS need it:
// read one env's binding, write one env's binding, and say where the answer
// came from.
//
// Declared here at the consumer rather than exported from an implementation
// package, per the package-boundary rule the repo already follows for
// clusterImageLister and envTargetResolver in env_verify.go. All three
// consumers (deploy's digest resolver, promote, env verify) live in this
// package, so one declaration here is the consumer-side declaration — there is
// no second package to keep ignorant.
//
// THREE METHODS, AND THE REASON THERE IS NO FOURTH.
//
// A List/all-bindings method is the obvious candidate and is deliberately
// ABSENT: no consumer ranges over the ledger. Every production call site asks
// about ONE named env, because every command is scoped to one env. Adding List
// would model the file's shape (a map, sitting in memory after a whole-file
// read) rather than any consumer's need, and it would be the expensive method
// to implement over HTTP — a list endpoint, pagination, a consistency question
// — bought for zero callers. projectstore learned this the loud way: 16 of its
// 29 methods had zero callers before it was cut back. Add the method when a
// consumer needs it.
//
// NO projectDir ANYWHERE IN THIS INTERFACE. That is the load-bearing detail.
// A project directory is a FILE concept; an HTTP backend has none, and an
// interface whose every method takes one is a file API wearing an interface's
// clothes — the second backend would have to accept and ignore a parameter,
// which is the shape that proves the seam was never real. The backing is bound
// ONCE at construction (newFileBindingStore takes the projectDir; a hosted
// constructor would take a base URL and a token), and the methods then speak
// only in domain terms: an env name and a binding.
type bindingStore interface {
	// Binding returns the release bound to env, and whether one exists.
	// "Not bound" is a normal, expected state — an env that was never
	// promoted — so it is a bool rather than an error, keeping it distinct
	// from a backend that could not be read at all.
	Binding(env string) (EnvBinding, bool, error)

	// SetBinding records that env runs this release, replacing any previous
	// binding for that env.
	//
	// Scoped to ONE env rather than taking the whole ledger because that is
	// what promotion actually is — a single pointer move — and because a
	// whole-ledger write is a read-modify-write that races. The file
	// backend still rewrites the file (one env's binding is not separately
	// addressable on disk), but that is the backend's business; a hosted
	// backend can make this a single conditional update precisely because
	// the interface asked for one env.
	SetBinding(env string, binding EnvBinding) error

	// Location names where bindings are recorded, for human-facing output
	// ("Binding: /repo/.forge/env-releases.json"). It is an opaque label —
	// a path today, a URL for a hosted backend — and callers must only
	// PRINT it. Joining it, opening it, or passing it to filepath would
	// reintroduce the file assumption this interface exists to remove.
	//
	// It earns its place by being the last thing that would otherwise force
	// a consumer to call envReleasesPath(projectDir) directly: promote's
	// output names the ledger, and without this the file concept leaks back
	// into the consumer that is supposed to be free of it.
	Location() string
}

// fileBindingStore is the default backend: the binding ledger as a JSON file
// at .forge/env-releases.json inside the project.
//
// The format and location are FROZEN. This file is committed in real projects
// (it is how a team reviews "prod moved to v1.5.13" in a PR), so a change to
// either is a breaking change for every existing user. It therefore delegates
// to the same ReadEnvReleases/WriteEnvReleases helpers that have always
// written it, rather than re-implementing the encoding alongside them.
type fileBindingStore struct {
	projectDir string
}

// newFileBindingStore binds a store to a project directory.
//
// This is the ONLY place projectDir enters the binding path, which is what
// keeps it out of the interface. Returns the concrete type, not the interface:
// accept interfaces, return structs.
func newFileBindingStore(projectDir string) fileBindingStore {
	return fileBindingStore{projectDir: projectDir}
}

// Binding reads one env's binding out of the ledger file. A missing file is an
// empty ledger, not an error — an unpromoted project has simply never written
// one, and failing there would make `forge env deploy` require a promote.
func (s fileBindingStore) Binding(env string) (EnvBinding, bool, error) {
	er, err := ReadEnvReleases(s.projectDir)
	if err != nil {
		return EnvBinding{}, false, err
	}
	b, ok := er.Bindings[env]
	return b, ok, nil
}

// SetBinding writes one env's binding, preserving every other env's.
//
// The read-modify-write is what preserves the others: the ledger is one file
// holding all envs, so writing only the new binding would silently drop the
// rest. Re-reading immediately before the write (rather than caching the
// ledger on the struct) keeps the window in which a concurrent promote of a
// DIFFERENT env can be lost as small as this backend can make it.
func (s fileBindingStore) SetBinding(env string, binding EnvBinding) error {
	er, err := ReadEnvReleases(s.projectDir)
	if err != nil {
		return err
	}
	er.Bindings[env] = binding
	return WriteEnvReleases(s.projectDir, *er)
}

// Location returns the ledger's path, for display.
func (s fileBindingStore) Location() string {
	return envReleasesPath(s.projectDir)
}

// bindingStoreFor returns the binding store a command should use for a
// project. This is the SINGLE place the backend is chosen, and it is what
// makes "add a hosted backend without changing any caller" true rather than
// aspirational: when a hosted backend lands, the config check and the
// alternate construction go HERE, and every consumer keeps compiling
// unchanged.
//
// It takes projectDir because choosing a backend is inherently a
// project-scoped question (it will read the project's config to decide), and
// because the file backend needs it. Note that the projectDir stops at this
// boundary — it is consumed in constructing the store and never reappears in
// the bindingStore interface the consumers hold.
func bindingStoreFor(projectDir string) bindingStore {
	return newFileBindingStore(projectDir)
}
