//go:build e2e

package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestE2EScaffoldFrontendEnumEntityBuilds is the acceptance gate for
// enum-typed entity fields in born frontend form pages.
//
// Scenario: a fresh `--frontend dashboard` project with ONE
// `// forge:entity` message carrying a same-package enum status field
// (the proto skill's taught convention — entity birth maps it to a
// TEXT + CHECK column) plus plain string/int fields. `forge scaffold`
// births the table, injects the CRUD quintet, and scaffolds the CRUD
// pages; then the generated app must pass the same gates `forge build`
// applies: `go build ./...` for the Go half and the frontend
// type-check (`tsc --noEmit`) for the dashboard half.
//
// This is RED before the typed-select fix, in two independent ways:
//
//  1. The create page projected the enum as `status: z.string()` + a
//     text input, so `mutation.mutate({...values})` failed the frontend
//     type-check: "Type 'string' is not assignable to type
//     'BrandStatus | undefined'" (MessageInit wants the TS enum).
//  2. The edit page was born completely EMPTY (`z.object({})`,
//     `values: item ? {} : undefined`, `updateMask: { paths: [] }`):
//     the AIP-134 update request wraps the entity, and the entity
//     message only exists in the descriptor's deep Schemas map — the
//     old Messages-only lookup silently bailed out.
//
// Content assertions use t.Errorf (not Fatalf) so a broken state still
// reaches the type-check step and surfaces the compiler error alongside
// the content failures.
//
// node/npm are required via requireTool (skip on a laptop, FAIL in CI),
// mirroring TestE2EScaffoldFrontendBuilds.
func TestE2EScaffoldFrontendEnumEntityBuilds(t *testing.T) {
	t.Parallel() // independent project in its own t.TempDir; binary shared via sync.Once
	requireTool(t, "node", "npm")

	forgeBin := buildforgeBinary(t)
	dir := t.TempDir()

	runCmd(t, dir, forgeBin,
		"project", "new", "brandapp",
		"--mod", "example.com/brandapp",
		"--service", "brand",
		"--frontend", "dashboard",
	)
	projectDir := filepath.Join(dir, "brandapp")
	addCorpusForgePkgReplace(t, projectDir)

	// Author the entity WITH an enum status field, per the proto skill's
	// enum conventions (UNSPECIFIED zero value, enum-name prefix,
	// declared alongside the entity message).
	protoPath := filepath.Join(projectDir, "proto", "services", "brand", "v1", "brand.proto")
	proto := readFileE2E(t, protoPath)
	proto += `
// forge:entity
message Brand {
  string id = 1;
  string name = 2;
  string tagline = 3;
  int32 priority = 4;
  BrandStatus status = 5;
}

enum BrandStatus {
  BRAND_STATUS_UNSPECIFIED = 0;
  BRAND_STATUS_DRAFT = 1;
  BRAND_STATUS_ACTIVE = 2;
  BRAND_STATUS_RETIRED = 3;
}
`
	if err := os.WriteFile(protoPath, []byte(proto), 0o644); err != nil {
		t.Fatalf("author brand proto: %v", err)
	}

	// Births the brands table (TEXT + CHECK for status), injects the
	// CRUD quintet, and runs the full generate pipeline — including the
	// TS stubs and the born CRUD pages.
	runCmd(t, projectDir, forgeBin, "scaffold")

	appDir := filepath.Join(projectDir, "frontends", "dashboard", "src", "app")

	// ── Born create page: every field present, enum as a typed select ──
	createPath := filepath.Join(appDir, "brands", "new", "page.tsx")
	assertPathExistsE2E(t, createPath)
	create := readFileE2E(t, createPath)
	for _, want := range []string{
		`import { BrandStatus } from "@/gen/services/brand/v1/brand_pb";`,
		// CREATE refuses the zero value: UNSPECIFIED is not a state the
		// domain has, so the form makes the author pick one. (The EDIT
		// page below carries the same schema WITHOUT the refine — an
		// existing row is allowed to still hold the sentinel.)
		`status: z.coerce.number().pipe(z.nativeEnum(BrandStatus)).refine((v) => v !== 0, "Required"),`,
		`{...register("name")}`,
		`{...register("tagline")}`,
		`{...register("priority")}`,
		`{...register("status")}`,
		"<select",
		"<option value={ BrandStatus.ACTIVE }>Active</option>",
	} {
		if !strings.Contains(create, want) {
			t.Errorf("born create page missing %q:\n%s", want, create)
		}
	}
	if strings.Contains(create, "status: z.string()") {
		t.Errorf("born create page types the enum as z.string() — guaranteed MessageInit type error at mutate():\n%s", create)
	}

	// ── Born edit page: NOT empty — carries the same editable fields,
	// and the AIP-134 mask names exactly those fields ──
	editPath := filepath.Join(appDir, "brands", "[id]", "edit", "page.tsx")
	assertPathExistsE2E(t, editPath)
	edit := readFileE2E(t, editPath)
	for _, want := range []string{
		`import { BrandStatus } from "@/gen/services/brand/v1/brand_pb";`,
		"status: z.coerce.number().pipe(z.nativeEnum(BrandStatus)),",
		"status: item.status ?? BrandStatus.UNSPECIFIED,",
		`{...register("name")}`,
		`{...register("tagline")}`,
		`{...register("priority")}`,
		`{...register("status")}`,
		"<select",
		`updateMask: { paths: ["name", "tagline", "priority", "status"] },`,
	} {
		if !strings.Contains(edit, want) {
			t.Errorf("born edit page missing %q (the empty-edit-form bug births `z.object({})` here):\n%s", want, edit)
		}
	}
	if strings.Contains(edit, "const schema = z.object({\n});") {
		t.Errorf("born edit page has an EMPTY form schema:\n%s", edit)
	}

	// ── The `forge build` gates: Go half… ──
	runCmd(t, projectDir, "go", "build", "./...")
	runCmd(t, projectDir, "go", "vet", "./...")

	// ── …and the frontend half. `npx tsc --noEmit` is the type-check
	// `next build` applies; it is exactly the gate the pre-fix enum
	// projection failed with the MessageInit error. ──
	webDir := filepath.Join(projectDir, "frontends", "dashboard")
	runCmdTimeout(t, webDir, 5*time.Minute,
		"npm", "install", "--no-audit", "--no-fund", "--prefer-offline")
	runCmdTimeout(t, webDir, 3*time.Minute,
		"npx", "tsc", "--noEmit")
}
