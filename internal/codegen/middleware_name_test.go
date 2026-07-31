package codegen

import (
	"os"
	"path/filepath"
	"testing"
)

// TestResolveMiddlewareWrappers_Core exercises the pure naming core directly —
// the multi-impl disambiguation and same-concrete fallback that the generate
// pipeline itself cannot currently reach (it emits exactly ONE wrapped
// constructor per package, `New`; see the note in TestResolveMiddlewareWrapper_Dir).
func TestResolveMiddlewareWrappers_Core(t *testing.T) {
	t.Run("single impl keeps the clean name and bare op namespace", func(t *testing.T) {
		got := resolveMiddlewareWrappers([]wrappedConstructor{{Ctor: "New", Concrete: "service"}})
		want := MiddlewareWrapper{
			Constructor: "NewServiceWithForgeMiddleware",
			Struct:      "forgeMiddlewareService",
			OpSegment:   "", // single → op stays "<pkg>.<Method>"
		}
		if got["New"] != want {
			t.Fatalf("single-impl wrapper = %+v, want %+v", got["New"], want)
		}
	})

	t.Run("two impls of one interface get DISTINCT concrete-keyed names + namespaces", func(t *testing.T) {
		got := resolveMiddlewareWrappers([]wrappedConstructor{
			{Ctor: "New", Concrete: "service"},
			{Ctor: "NewReadOnly", Concrete: "readOnlyService"},
		})
		newW := got["New"]
		roW := got["NewReadOnly"]
		if newW.Constructor != "NewServiceWithForgeMiddleware" || newW.Struct != "forgeMiddlewareService" {
			t.Errorf("New wrapper = %+v", newW)
		}
		if roW.Constructor != "NewReadOnlyServiceWithForgeMiddleware" || roW.Struct != "forgeMiddlewareReadOnlyService" {
			t.Errorf("NewReadOnly wrapper = %+v", roW)
		}
		// Distinct constructor names (the whole point — no collision).
		if newW.Constructor == roW.Constructor {
			t.Errorf("constructor names collide: %q", newW.Constructor)
		}
		// Multi-impl → each carries a distinct op-namespace segment so the impls
		// are distinguishable in traces/metrics.
		if newW.OpSegment != "Service" || roW.OpSegment != "ReadOnlyService" {
			t.Errorf("op segments = %q / %q, want Service / ReadOnlyService", newW.OpSegment, roW.OpSegment)
		}
		if newW.OpSegment == roW.OpSegment {
			t.Errorf("op segments collide: %q", newW.OpSegment)
		}
	})

	t.Run("two constructors of the SAME concrete type fall back to the constructor name", func(t *testing.T) {
		got := resolveMiddlewareWrappers([]wrappedConstructor{
			{Ctor: "New", Concrete: "service"},
			{Ctor: "NewReadOnly", Concrete: "service"}, // same concrete → concrete keying would collide
		})
		newW := got["New"]
		roW := got["NewReadOnly"]
		// Fallback keys off the (unique) constructor name — not New<Ctor>...,
		// the constructor already begins with "New".
		if newW.Constructor != "NewWithForgeMiddleware" {
			t.Errorf("New fallback constructor = %q, want NewWithForgeMiddleware", newW.Constructor)
		}
		if roW.Constructor != "NewReadOnlyWithForgeMiddleware" {
			t.Errorf("NewReadOnly fallback constructor = %q, want NewReadOnlyWithForgeMiddleware", roW.Constructor)
		}
		if newW.Constructor == roW.Constructor {
			t.Errorf("fallback constructor names collide: %q", newW.Constructor)
		}
		if newW.Struct == roW.Struct {
			t.Errorf("fallback struct names collide: %q", newW.Struct)
		}
	})

	t.Run("unresolvable concrete type degrades to the constructor name", func(t *testing.T) {
		// Sole constructor, concrete unresolved (e.g. `return newImpl()` / local
		// var) → constructor-name keying, never a hard error.
		got := resolveMiddlewareWrappers([]wrappedConstructor{{Ctor: "New", Concrete: ""}})
		if got["New"].Constructor != "NewWithForgeMiddleware" {
			t.Fatalf("unresolvable concrete constructor = %q, want NewWithForgeMiddleware", got["New"].Constructor)
		}
	})
}

// TestResolveMiddlewareWrapper_Dir proves the dir-based resolver reads the
// CONCRETE type from the constructor's return expression (`return &service{…}`),
// including the fallible `(Service, error)` shape, and that the canonical
// single-`New` package title-cases `service` back to `NewServiceWithForgeMiddleware`
// with a bare op namespace — i.e. the common case is unchanged by concrete-type
// keying.
//
// NOTE ON REACHABILITY: the generate pipeline wraps exactly one constructor per
// package (generate_middleware calls WriteObservedDecorator once, for `New`;
// compose constructs one component per package via `New`). So the multi-wrapped-
// constructor scenario above is NOT reachable through the real pipeline today —
// hence it is tested against the pure core directly. The dir resolver
// deliberately keys only off `New`, immune to unrelated sibling `New*` funcs.
func TestResolveMiddlewareWrapper_Dir(t *testing.T) {
	cases := []struct {
		name string
		src  string
		want MiddlewareWrapper
	}{
		{
			name: "non-fallible New returning &service",
			src: "package checkout\n\ntype Service interface{ Do() }\n" +
				"type service struct{}\n\nfunc (s *service) Do() {}\n" +
				"type Deps struct{}\n\nfunc New(d Deps) Service { return &service{} }\n",
			want: MiddlewareWrapper{Constructor: "NewServiceWithForgeMiddleware", Struct: "forgeMiddlewareService", OpSegment: ""},
		},
		{
			name: "fallible New with an error branch before the real return",
			src: "package checkout\n\nimport \"errors\"\n\ntype Service interface{ Do() }\n" +
				"type service struct{}\n\nfunc (s *service) Do() {}\n" +
				"type Deps struct{ Bad bool }\n\n" +
				"func New(d Deps) (Service, error) {\n\tif d.Bad {\n\t\treturn nil, errors.New(\"bad\")\n\t}\n\treturn &service{}, nil\n}\n",
			want: MiddlewareWrapper{Constructor: "NewServiceWithForgeMiddleware", Struct: "forgeMiddlewareService", OpSegment: ""},
		},
		{
			name: "concrete type other than `service` keys the wrapper off it",
			src: "package checkout\n\ntype Service interface{ Do() }\n" +
				"type engine struct{}\n\nfunc (e *engine) Do() {}\n" +
				"type Deps struct{}\n\nfunc New(d Deps) Service { return &engine{} }\n",
			want: MiddlewareWrapper{Constructor: "NewEngineWithForgeMiddleware", Struct: "forgeMiddlewareEngine", OpSegment: ""},
		},
		{
			name: "unresolvable return (delegated call) degrades to constructor name",
			src: "package checkout\n\ntype Service interface{ Do() }\n" +
				"type service struct{}\n\nfunc (s *service) Do() {}\n" +
				"func build() Service { return &service{} }\n" +
				"type Deps struct{}\n\nfunc New(d Deps) Service { return build() }\n",
			want: MiddlewareWrapper{Constructor: "NewWithForgeMiddleware", Struct: "forgeMiddlewareNew", OpSegment: ""},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			if err := os.WriteFile(filepath.Join(dir, "contract.go"), []byte(tc.src), 0o644); err != nil {
				t.Fatalf("write: %v", err)
			}
			got := ResolveMiddlewareWrapper(dir, "Service")
			if got != tc.want {
				t.Fatalf("ResolveMiddlewareWrapper = %+v, want %+v", got, tc.want)
			}
		})
	}
}
