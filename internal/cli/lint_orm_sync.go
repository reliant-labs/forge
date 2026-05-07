// File: internal/cli/lint_orm_sync.go
//
// proto-orm-out-of-sync — surfaces the "ran buf generate without forge
// generate" pitfall. When the user adds an entity proto under
// `proto/db/v1/`, `buf generate` produces the `.pb.go` stubs but the
// forge ORM pass is what produces the matching `.pb.orm.go` files. Users
// who treat `buf generate` as the canonical command end up with stale or
// missing ORM code that fails to compile against `internal/db/`'s typed
// queries; the failure points at "missing method on *XOrm" rather than
// the actual root cause.
//
// This lint detects two staleness shapes in `gen/db/v1/`:
//
//   1. A `<base>.pb.go` exists but no matching `<base>_*.pb.orm.go` is
//      present at all (buf ran, forge generate hasn't).
//
//   2. `<base>.pb.go` is newer than every `<base>_*.pb.orm.go` sibling
//      (proto was edited and buf re-ran, but forge generate didn't).
//
// Output is a single warning per file plus a one-line remediation.
// The lint is non-fatal — `forge lint` keeps walking and the project
// build itself will fail loudly if the staleness actually breaks
// compilation.

package cli

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// runORMSyncLint scans gen/db/v1/ for proto-vs-ORM mtime drift.
// Returns nil whenever the project doesn't have a gen/db/v1/ tree (e.g.
// CLI projects, projects without entities). Findings are printed to
// stderr and contribute a warning, not a non-zero exit.
func runORMSyncLint(projectDir string) error {
	dir := filepath.Join(projectDir, "gen", "db", "v1")
	info, err := os.Stat(dir)
	if err != nil || !info.IsDir() {
		return nil
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		return fmt.Errorf("read %s: %w", dir, err)
	}

	// Group siblings by base name (e.g. "stripe_entities" groups
	// stripe_entities.pb.go and stripe_entities_customer.pb.orm.go,
	// stripe_entities_subscription.pb.orm.go, ...).
	type sibling struct {
		pbGoMtime  time.Time
		pbGoExists bool
		ormPaths   []string
		ormLatest  time.Time
	}
	groups := map[string]*sibling{}

	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		// Skip files that aren't part of the proto/orm shape.
		if !strings.HasSuffix(name, ".pb.go") && !strings.HasSuffix(name, ".pb.orm.go") {
			continue
		}
		path := filepath.Join(dir, name)
		fi, statErr := os.Stat(path)
		if statErr != nil {
			continue
		}

		// Strip suffix to derive the group key.
		var base string
		isORM := strings.HasSuffix(name, ".pb.orm.go")
		if isORM {
			// foo_entity.pb.orm.go -> base = "foo" if the file is foo_<entity>.pb.orm.go
			// for the canonical layout. Strip ".pb.orm.go" then trim the
			// trailing "_<entity>" suffix iff the result has a stem that
			// matches an existing or to-be-found <stem>.pb.go. We can't
			// know the proto stem from filename alone (entity names contain
			// underscores too), so we group on every prefix-of-path
			// candidate and let the shape of the directory disambiguate
			// the proto stem during the report pass.
			//
			// Simplification: instead of trimming, store the ORM file
			// against its full base-without-".pb.orm.go" key; we'll match
			// against pb.go basenames at report time by prefix.
			base = strings.TrimSuffix(name, ".pb.orm.go")
		} else {
			// foo.pb.go -> base = "foo"
			base = strings.TrimSuffix(name, ".pb.go")
			// Skip the orm-shared file (foo_orm_shared.pb.go shouldn't
			// exist, but defensive) — only the buf-emitted stub matters
			// for staleness.
		}
		if _, ok := groups[base]; !ok {
			groups[base] = &sibling{}
		}
		if isORM {
			s := groups[base]
			s.ormPaths = append(s.ormPaths, path)
			if fi.ModTime().After(s.ormLatest) {
				s.ormLatest = fi.ModTime()
			}
		} else {
			groups[base] = &sibling{
				pbGoExists: true,
				pbGoMtime:  fi.ModTime(),
			}
		}
	}

	// Re-link orm groups (which keyed on the full ORM stem
	// "foo_<entity>") to their parent pb.go group ("foo"). For each ORM
	// group key, find the longest pb.go base that is a prefix.
	pbBases := []string{}
	for k, g := range groups {
		if g.pbGoExists {
			pbBases = append(pbBases, k)
		}
	}
	sort.Slice(pbBases, func(i, j int) bool {
		return len(pbBases[i]) > len(pbBases[j])
	})

	pbWithORM := map[string][]string{}
	pbORMLatest := map[string]time.Time{}
	for k, g := range groups {
		if g.pbGoExists {
			continue
		}
		// k is an ORM stem like "foo_entity". Find the longest pb.go
		// base that is a prefix followed by "_".
		for _, pb := range pbBases {
			if k == pb || strings.HasPrefix(k, pb+"_") {
				pbWithORM[pb] = append(pbWithORM[pb], g.ormPaths...)
				if g.ormLatest.After(pbORMLatest[pb]) {
					pbORMLatest[pb] = g.ormLatest
				}
				break
			}
		}
	}

	// Now report: for each pb.go base, check for missing-or-stale ORM.
	var findings []string
	for _, base := range pbBases {
		g := groups[base]
		// Skip the descriptor stub file (gen/db/v1/forge_descriptor.pb.go
		// doesn't exist, but other forge-internal pb.go files might
		// emerge in the future — be conservative).
		if base == "" {
			continue
		}
		ormPaths := pbWithORM[base]
		if len(ormPaths) == 0 {
			// If the proto has no entity messages, there's correctly no
			// .pb.orm.go output. Heuristically, only warn if the base
			// name suggests an entities file (a default forge naming
			// convention) OR the proto file has any *_entity / _entities
			// hint in the path. Since we don't parse the proto here,
			// fall back to: warn unconditionally for files in proto/db/v1
			// that have no orm sibling.
			findings = append(findings, fmt.Sprintf(
				"  ⚠️  gen/db/v1/%s.pb.go has no matching *.pb.orm.go sibling — did you run `buf generate` without `forge generate`?",
				base))
			continue
		}
		latest := pbORMLatest[base]
		if g.pbGoMtime.After(latest.Add(time.Second)) {
			findings = append(findings, fmt.Sprintf(
				"  ⚠️  gen/db/v1/%s.pb.go is newer than its *.pb.orm.go siblings (proto was regenerated without forge ORM pass).",
				base))
		}
	}

	if len(findings) == 0 {
		return nil
	}

	fmt.Println()
	fmt.Println("proto-orm-out-of-sync:")
	for _, f := range findings {
		fmt.Println(f)
	}
	fmt.Println("  Remediation: run `forge generate` (it invokes buf generate then the ORM, descriptor, mock, and bootstrap passes).")
	fmt.Println("  See: forge skill load proto")
	return nil
}
