package scaffolds

import (
	"path/filepath"
	"testing"
)

func TestBannerLintRoot_MissingTier1(t *testing.T) {
	t.Parallel()
	res, err := BannerLintRoot(filepath.Join("testdata", "banners", "missing_tier1"))
	if err != nil {
		t.Fatalf("BannerLintRoot returned error: %v", err)
	}
	if !findingMatches(res.Findings, "banner-tier1-missing-generated-header") {
		t.Fatalf("expected a banner-tier1-missing-generated-header finding, got: %+v", res.Findings)
	}
}

func TestBannerLintRoot_MissingTier2(t *testing.T) {
	t.Parallel()
	res, err := BannerLintRoot(filepath.Join("testdata", "banners", "missing_tier2"))
	if err != nil {
		t.Fatalf("BannerLintRoot returned error: %v", err)
	}
	if !findingMatches(res.Findings, "banner-tier2-missing-scaffold-header") {
		t.Fatalf("expected a banner-tier2-missing-scaffold-header finding, got: %+v", res.Findings)
	}
}

func TestBannerLintRoot_Correct(t *testing.T) {
	t.Parallel()
	res, err := BannerLintRoot(filepath.Join("testdata", "banners", "correct"))
	if err != nil {
		t.Fatalf("BannerLintRoot returned error: %v", err)
	}
	if len(res.Findings) != 0 {
		t.Fatalf("expected zero findings on correct fixture, got %d: %+v", len(res.Findings), res.Findings)
	}
}

func TestClassifyTemplate(t *testing.T) {
	t.Parallel()
	cases := []struct {
		rel  string
		want templateTier
	}{
		{"internal/templates/service/handlers_gen.go.tmpl", tier1Generated},
		{"internal/templates/frontend/hooks.ts.tmpl", tier1Generated},
		// The writer is the authority on tier (internal/codegen/writers.go).
		// writeForgeOwned => Tier-1: the cmd-tree files, the per-service item
		// file, the three group anchors, and the mount surface.
		{"internal/templates/project/cmd-svc-group.go.tmpl", tier1Generated},
		{"internal/templates/project/cmd-worker-register.go.tmpl", tier1Generated},
		{"internal/templates/project/mounts_services.go.tmpl", tier1Generated},
		// root.go stays Tier-1 on merit: it declares ServiceName from the
		// forge.yaml project name and gates its newDBCmd wiring on a config
		// field parsed out of the user's config proto — genuinely re-derived
		// from user input, which is the only thing that justifies Tier-1.
		{"internal/templates/project/cmd-tree-root.go.tmpl", tier1Generated},
		// db-source.go is the re-derived sliver carved out of db.go: whether
		// forgedb.MigrationsFS is even a referenceable symbol depends on
		// whether db/migrations/ holds any .sql, re-read from disk each run.
		{"internal/templates/project/cmd-tree-db-source.go.tmpl", tier1Generated},
		// writeForgeScaffoldOnce => one-shot scaffold: the composition root,
		// the per-worker/per-operator subcommand items, and the internal/app
		// composition pair. Forge writes each once and never rewrites it, so
		// certifying them Tier-1 asked their authors for a DO-NOT-EDIT banner
		// on files forge never regenerates.
		{"internal/templates/project/cmd-main.go.tmpl", tier2Scaffold},
		// Four command-tree files joined this tier: every statement in each is
		// a DECISION the application owns, and each one's invariant half now
		// lives in a forge library — which is what keeps an owned copy on the
		// upgrade path instead of stranding it.
		//
		//   serve.go   auth posture, interceptor order, payload caps, CORS,
		//              readiness, teardown            → pkg/serverkit
		//   server.go  the all-services ServeSpec     → pkg/serverkit
		//   version.go the ldflags-stamped variables  → pkg/cmdkit
		//   db.go      migration policy               → pkg/migratekit
		{"internal/templates/project/cmd-tree-serve.go.tmpl", tier2Scaffold},
		{"internal/templates/project/cmd-tree-server.go.tmpl", tier2Scaffold},
		{"internal/templates/project/cmd-tree-version.go.tmpl", tier2Scaffold},
		{"internal/templates/project/cmd-tree-db.go.tmpl", tier2Scaffold},
		{"internal/templates/project/cmd-worker-group.go.tmpl", tier2Scaffold},
		{"internal/templates/project/cmd-operator-group.go.tmpl", tier2Scaffold},
		{"internal/templates/project/compose.go.tmpl", tier2Scaffold},
		{"internal/templates/project/lifecycle.go.tmpl", tier2Scaffold},
		// CI workflows are write-once scaffolds the user owns (no
		// forge:hash marker), not Tier-1 regenerated files.
		{"internal/templates/ci/github/ci.yml.tmpl", tier2Scaffold},
		{"internal/templates/ci/github/deploy.yml.tmpl", tier2Scaffold},
		{"internal/templates/ci/github/dependabot.yml.tmpl", tier2Scaffold},
		{"internal/templates/internal-package/contract.go.tmpl", tier2Scaffold},
		{"internal/templates/frontend/pages/list-page.tsx.tmpl", tier2Scaffold},
		{"internal/templates/project/providers.go.tmpl", tier3UserOwned},
		{"internal/templates/project/app-auth.go.tmpl", tier3UserOwned},
		{"internal/templates/service/service.go.tmpl", tier3UserOwned},
		{"internal/templates/worker/worker.go.tmpl", tier3UserOwned},
		{"internal/templates/project/Makefile.tmpl", tierSkip},
		{"internal/templates/project/go.mod.tmpl", tierSkip},
		{"internal/templates/project/Dockerfile.tmpl", tierSkip},
		// Method-body FRAGMENTS appended into user-owned scaffolds carry no
		// file header of their own and are skip-listed, not unclassified.
		{"internal/templates/service/handlers_methods.go.tmpl", tierSkip},
		{"internal/templates/service/handlers_crud_shim_method.go.tmpl", tierSkip},
	}
	for _, c := range cases {
		got := classifyTemplate(c.rel)
		if got != c.want {
			t.Errorf("classifyTemplate(%q) = %d, want %d", c.rel, got, c.want)
		}
	}
}
