package cli

// A command-scoped memo of env declaration renders.
//
// WHY. An evaluation of a large env's KCL is seconds of CPU (control-plane's
// prod: ~4s), and a read-only command reaches RenderKCL from many places that
// each only need one fact of the declaration: the lifecycle, where the env
// runs, which ledger holds its promotions, its control plane, its cluster and
// its namespace. `forge env status` with no environment made THREE renders
// per env before it read a single promotion, and SIX for a bound hosted env
// (the rollout read's control-plane lookup and the cluster/namespace pair
// each rendered again) — 37s on control-plane's four envs, where the web's
// environment view gives up long before. Threading the entities through every
// one of those helpers would touch a dozen signatures that other commands
// share; the declaration is the same bytes every time, so the render is
// shared instead.
//
// SCOPE IS OPT-IN, per context. Only a command that installs a memo
// (withDeclarationMemo) shares renders, and it may install one only when
// nothing it does changes what a render returns: it arms no dev stack, binds
// no release, sets no -D option. The key still carries every input forge
// knows changes the output (the remembered render context, the release
// bindings, the -D options) as a backstop, so a scope that broke the rule
// would re-render rather than read a stale declaration.
//
// RENDERS OF DIFFERENT ENVS RUN CONCURRENTLY, bounded: each is a `forge kcl
// eval` child of seconds of CPU and hundreds of MB, so a project with many
// envs must not start them all at once. Renders of the SAME env wait for the
// first and share its result — including its error, which is the
// declaration's answer — except a cancellation, which is the caller's and
// leaves the next caller free to render.

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync"

	"github.com/reliant-labs/forge/internal/kclrender"
)

// declarationRenderParallelism bounds the concurrent evaluations one memo
// runs. Four keeps a four-env project (the common shape: dev, staging,
// prod and one more) fully parallel without letting a project of twenty
// preview envs start twenty evaluations at once on a laptop.
const declarationRenderParallelism = 4

type declarationMemoKey struct{}

// declarationMemo shares RenderKCL results within one command.
type declarationMemo struct {
	mu      sync.Mutex
	entries map[string]*declarationMemoEntry
	slots   chan struct{}
}

type declarationMemoEntry struct {
	mu       sync.Mutex
	done     bool
	entities *KCLEntities
	err      error
}

// withDeclarationMemo returns ctx carrying a fresh memo. A ctx that already
// carries one is returned unchanged, so nested scopes share the outer memo.
func withDeclarationMemo(ctx context.Context) context.Context {
	if declarationMemoFrom(ctx) != nil {
		return ctx
	}
	return context.WithValue(ctx, declarationMemoKey{}, &declarationMemo{
		entries: map[string]*declarationMemoEntry{},
		slots:   make(chan struct{}, declarationRenderParallelism),
	})
}

func declarationMemoFrom(ctx context.Context) *declarationMemo {
	if ctx == nil {
		return nil
	}
	m, _ := ctx.Value(declarationMemoKey{}).(*declarationMemo)
	return m
}

// declarationMemoKeyFor is everything that decides a plain RenderKCL's
// output that forge holds in process state.
func declarationMemoKeyFor(ctx context.Context, projectDir, env string) string {
	root, err := filepath.Abs(projectDir)
	if err != nil {
		root = projectDir
	}
	root = filepath.Clean(root)
	return strings.Join([]string{
		root, env,
		fmt.Sprintf("%+v", rememberedKCLContext(root, env)),
		strings.Join(kclrender.ReleaseDArgs(ctx), "\x00"),
		strings.Join(activeRenderOptionDArgs(), "\x00"),
	}, "\x01")
}

// render returns the env's declaration, evaluating it at most once per key.
func (m *declarationMemo) render(ctx context.Context, projectDir, env string, eval func(context.Context) (*KCLEntities, error)) (*KCLEntities, error) {
	key := declarationMemoKeyFor(ctx, projectDir, env)
	m.mu.Lock()
	e, ok := m.entries[key]
	if !ok {
		e = &declarationMemoEntry{}
		m.entries[key] = e
	}
	m.mu.Unlock()

	// Held across the evaluation, so a second caller for the same env
	// waits for the first rather than evaluating beside it. Callers for
	// other envs hold other entries and are not blocked.
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.done {
		return e.entities, e.err
	}
	select {
	case m.slots <- struct{}{}:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	entities, err := eval(ctx)
	<-m.slots
	if err != nil && (ctx.Err() != nil || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)) {
		// The caller's deadline, not the declaration's answer: the next
		// caller (with its own budget) renders afresh.
		return nil, err
	}
	e.done, e.entities, e.err = true, entities, err
	return entities, err
}
