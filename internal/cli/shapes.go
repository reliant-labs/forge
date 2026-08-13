package cli

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/spf13/cobra"
)

// `forge project shapes` — the symbol index an agent needs for recon, derived
// LIVE from the sources on every invocation.
//
// ── Why this exists ──────────────────────────────────────────────────────
//
// Measured across two forge-one-shot runs: fan-out agents spent 28.4 minutes
// across 123 tool calls answering "where is X and what shape is it" with
// `find` / `grep` / `ls`. It was the single largest recoverable cost in the
// run, larger than any generator gap. The dominant pattern was paging one
// 1,685-line proto by symbol, one message at a time:
//
//	grep -n "message ListMaterialsRequest" -A 10 proto/services/roofops/v1/roofops.proto
//	grep -n "message Material {" -A 20 ...
//
// Eleven agents each re-derived the same contract, and each grep cost a whole
// turn. One `shapes` call answers all of it.
//
// ── Why it does NOT read gen/forge_descriptor.json ───────────────────────
//
// The descriptor is a derived CACHE of the protos. A command whose whole job
// is to tell an agent the truth must not answer from a cache that can be
// stale — and that cache demonstrably could: renaming one RPC in it made
// `forge project graph` report an RPC present in no proto file. Parsing the
// sources costs milliseconds and cannot lie. Recon output that is confidently
// wrong is worse than no recon output, because nothing downstream re-checks it.
//
// ── Output ───────────────────────────────────────────────────────────────
//
// One symbol per line, greppable and diffable, always with file:line so the
// next step is a targeted read rather than another search:
//
//	rpc      roofops.RecalculateEstimate   proto/services/roofops/v1/roofops.proto:412  in=… out=…
//	message  Estimate                      proto/services/roofops/v1/roofops.proto:640  fields=14
//	enum     EstimateStatus                proto/services/roofops/v1/roofops.proto:604  values=5
//	table    estimates                     db/migrations/00004_create_estimates.up.sql:9
//	store    EstimateStore                 internal/db/store_gen.go:214  go doc ./internal/db EstimateStore
//	handler  roofops.RecalculateEstimate   internal/handlers/roofops/rpc_recalculate_estimate.go:24  unwired-stub
//	hook     useListEstimates              frontends/dashboard/src/hooks/roofops-service-hooks_gen.ts:212

type shape struct {
	Kind   string // rpc | message | enum | table | store | handler | hook
	Name   string
	File   string
	Line   int
	Detail string
}

var (
	reProtoRPC     = regexp.MustCompile(`^\s*rpc\s+(\w+)\s*\(\s*([\w.]+)\s*\)\s*returns\s*\(\s*([\w.]+)\s*\)`)
	reProtoMessage = regexp.MustCompile(`^message\s+(\w+)\s*\{`)
	reProtoEnum    = regexp.MustCompile(`^enum\s+(\w+)\s*\{`)
	reProtoService = regexp.MustCompile(`^service\s+(\w+)\s*\{`)
	reSQLTable     = regexp.MustCompile(`(?i)^\s*CREATE\s+TABLE\s+(?:IF\s+NOT\s+EXISTS\s+)?"?(\w+)"?`)
	reGoMethod     = regexp.MustCompile(`^func\s+\(s\s+\*Service\)\s+(\w+)\s*\(`)
	reUnwiredStub  = regexp.MustCompile(`forge:gen unwired-stub symbol=(\S+)`)
	reTSHook       = regexp.MustCompile(`^export\s+(?:const|function)\s+(use\w+)`)
	reStoreIface   = regexp.MustCompile(`^type\s+(\w*Store)\s+interface\s*\{`)
)

func newShapesCmd() *cobra.Command {
	var (
		grepPat string
		kinds   string
	)
	cmd := &cobra.Command{
		Use:   "shapes",
		Short: "List every API shape — RPCs, messages, enums, tables, handlers, hooks — with file:line",
		Long: `List every API shape in the project with its source location.

Derived LIVE from .proto, db/migrations and source on every call — never from
gen/forge_descriptor.json, which is a cache and can be stale. Answers "where is
X and what shape is it" in ONE call instead of a sequence of greps.

Start recon here. A measured fan-out spent 28 minutes across 123 calls
re-deriving this by hand, mostly paging one large proto by symbol.

--grep is a REGEX, so ask about every entity you own in ONE call. A later run
adopted this command and still invoked it once per entity — 11 turns of
--grep Property then --grep Customer — which is the old grep loop wearing a
new command's name.

Examples:
  forge project shapes                          # everything
  forge project shapes --grep Estimate          # one entity across all layers
  forge project shapes --grep 'Invoice|Payment' # SEVERAL entities in one call
  forge project shapes --kind rpc,handler       # what is declared vs implemented`,
		RunE: func(cmd *cobra.Command, args []string) error {
			projectDir, err := os.Getwd()
			if err != nil {
				return err
			}
			shapes := collectShapes(projectDir)

			var want map[string]bool
			if kinds != "" {
				want = map[string]bool{}
				for _, k := range strings.Split(kinds, ",") {
					want[strings.TrimSpace(k)] = true
				}
			}
			var pat *regexp.Regexp
			if grepPat != "" {
				pat, err = regexp.Compile("(?i)" + grepPat)
				if err != nil {
					return fmt.Errorf("invalid --grep pattern: %w", err)
				}
			}

			out := cmd.OutOrStdout()

			// Group by the alternation branch that matched, so ONE call asking
			// about several entities reads like several answers instead of one
			// undifferentiated wall. Without this the batched form is harder to
			// read than repeated calls, and a measured run duly made repeated
			// calls — 11 turns of --grep Property then --grep Customer.
			groups := grepBranches(grepPat)

			matched := make([][]shape, len(groups)+1)
			n := 0
			for _, s := range shapes {
				if want != nil && !want[s.Kind] {
					continue
				}
				if pat != nil && !pat.MatchString(s.Name) && !pat.MatchString(s.Detail) {
					continue
				}
				matched[branchIndex(groups, s)] = append(matched[branchIndex(groups, s)], s)
				n++
			}

			write := func(s shape) {
				loc := fmt.Sprintf("%s:%d", s.File, s.Line)
				if s.Detail != "" {
					fmt.Fprintf(out, "%-8s %-42s %-58s %s\n", s.Kind, s.Name, loc, s.Detail)
				} else {
					fmt.Fprintf(out, "%-8s %-42s %s\n", s.Kind, s.Name, loc)
				}
			}
			for i, g := range groups {
				if len(matched[i]) == 0 {
					continue
				}
				if len(groups) > 1 {
					fmt.Fprintf(out, "\n── %s ── %d shape(s)\n", g, len(matched[i]))
				}
				for _, s := range matched[i] {
					write(s)
				}
			}
			// Anything matching the pattern but no single branch (a detail-only
			// hit, or an un-split pattern) still prints — dropping it would make
			// the grouped form lossy, which is worse than ungrouped.
			if rest := matched[len(groups)]; len(rest) > 0 {
				if len(groups) > 1 {
					fmt.Fprintf(out, "\n── other ── %d shape(s)\n", len(rest))
				}
				for _, s := range rest {
					write(s)
				}
			}

			if n == 0 {
				fmt.Fprintln(out, "no shapes matched (is this a forge project with protos?)")
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&grepPat, "grep", "", "case-insensitive regex filter over name and detail")
	cmd.Flags().StringVar(&kinds, "kind", "", "comma-separated kinds: rpc,message,enum,table,store,handler,hook")
	return cmd
}

// collectShapes walks the sources. Every scan is a cheap line scan — the whole
// point is that this is fast enough to never need caching.
func collectShapes(projectDir string) []shape {
	var out []shape
	out = append(out, scanProtos(filepath.Join(projectDir, "proto"))...)
	out = append(out, scanMigrations(filepath.Join(projectDir, "db", "migrations"))...)
	out = append(out, scanStores(filepath.Join(projectDir, "internal", "db"))...)
	out = append(out, scanHandlers(filepath.Join(projectDir, "internal", "handlers"))...)
	out = append(out, scanHooks(filepath.Join(projectDir, "frontends"))...)

	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Kind != out[j].Kind {
			return kindRank(out[i].Kind) < kindRank(out[j].Kind)
		}
		return out[i].Name < out[j].Name
	})
	return out
}

func kindRank(k string) int {
	switch k {
	case "rpc":
		return 0
	case "handler":
		return 1
	case "message":
		return 2
	case "enum":
		return 3
	case "table":
		return 4
	case "store":
		return 5
	case "hook":
		return 6
	}
	return 7
}

// eachLine runs fn over every line of every file under root matching ext.
// Errors are skipped rather than reported: a shapes listing that fails because
// one file is unreadable is less useful than a partial one.
func eachLine(root, ext string, fn func(rel string, lineNo int, line string)) {
	_ = filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return nil //nolint:nilerr // partial output beats no output
		}
		if ext != "" && !strings.HasSuffix(path, ext) {
			return nil
		}
		if strings.Contains(path, "node_modules") || strings.Contains(path, "/.next/") {
			return filepath.SkipDir
		}
		f, openErr := os.Open(path)
		if openErr != nil {
			return nil //nolint:nilerr // same
		}
		defer func() { _ = f.Close() }()

		rel := path
		if wd, wdErr := os.Getwd(); wdErr == nil {
			if r, relErr := filepath.Rel(wd, path); relErr == nil {
				rel = r
			}
		}
		sc := bufio.NewScanner(f)
		sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
		for i := 1; sc.Scan(); i++ {
			fn(rel, i, sc.Text())
		}
		return nil
	})
}

func scanProtos(root string) []shape {
	var out []shape
	var svc string
	eachLine(root, ".proto", func(rel string, n int, line string) {
		if m := reProtoService.FindStringSubmatch(line); m != nil {
			svc = m[1]
			return
		}
		if m := reProtoRPC.FindStringSubmatch(line); m != nil {
			name := m[1]
			if svc != "" {
				name = svc + "." + m[1]
			}
			out = append(out, shape{"rpc", name, rel, n,
				fmt.Sprintf("in=%s out=%s", m[2], m[3])})
			return
		}
		if m := reProtoMessage.FindStringSubmatch(line); m != nil {
			out = append(out, shape{"message", m[1], rel, n, ""})
			return
		}
		if m := reProtoEnum.FindStringSubmatch(line); m != nil {
			out = append(out, shape{"enum", m[1], rel, n, ""})
		}
	})
	return out
}

func scanMigrations(root string) []shape {
	var out []shape
	eachLine(root, ".up.sql", func(rel string, n int, line string) {
		if m := reSQLTable.FindStringSubmatch(line); m != nil {
			out = append(out, shape{"table", m[1], rel, n, ""})
		}
	})
	return out
}

// scanHandlers reports the *Service methods that ARE the RPC handlers, and
// flags the ones forge stamped as unimplemented. The stub marker is the same
// one the fan-out work list is derived from, so "declared but not implemented"
// is answerable from this output alone.
func scanHandlers(root string) []shape {
	var out []shape
	pending := ""
	eachLine(root, ".go", func(rel string, n int, line string) {
		if strings.HasSuffix(rel, "_test.go") {
			return
		}
		if m := reUnwiredStub.FindStringSubmatch(line); m != nil {
			pending = m[1]
			return
		}
		if m := reGoMethod.FindStringSubmatch(line); m != nil {
			detail := ""
			if pending != "" {
				detail = "unwired-stub"
				pending = ""
			}
			out = append(out, shape{"handler", m[1], rel, n, detail})
		}
	})
	return out
}

// scanStores reports the generated persistence interfaces in internal/db —
// one `<Entity>Store` per entity plus the aggregate `Store` — which are
// what a service names as a Deps field.
//
// It is here because the alternative was measured: a run spent 11 turns
// reading forge's OWN generator source (internal/generator/plan_orm_store_gen.go,
// `git log` included) to work out what the generated store offers. That is
// the worst possible source for the answer — it is the code that WRITES the
// file, in a different repo, describing every project rather than this one.
// The generated file itself is right there and is specific to this schema.
//
// The detail column carries the file the reader should open, because the
// name alone still leaves "and what are its methods" unanswered; `go doc`
// does render an interface's method set in full, so unlike a struct it is a
// route that actually terminates.
func scanStores(root string) []shape {
	var out []shape
	eachLine(root, ".go", func(rel string, n int, line string) {
		if strings.HasSuffix(rel, "_test.go") {
			return
		}
		if m := reStoreIface.FindStringSubmatch(line); m != nil {
			out = append(out, shape{"store", m[1], rel, n, "go doc ./internal/db " + m[1]})
		}
	})
	return out
}

func scanHooks(root string) []shape {
	var out []shape
	eachLine(root, "-hooks_gen.ts", func(rel string, n int, line string) {
		if m := reTSHook.FindStringSubmatch(line); m != nil {
			out = append(out, shape{"hook", m[1], rel, n, ""})
		}
	})
	return out
}

// grepBranches splits a --grep pattern on TOP-LEVEL alternation so a batched
// query can be reported per entity. `Invoice|Payment` yields two branches;
// anything carrying regex metacharacters beyond a plain alternation yields one
// branch (the whole pattern), because splitting those would mis-attribute.
//
// Grouping exists to make the batched call the EASIER one to read. The command
// already accepted a regex, and a measured run still asked one entity per turn
// — a flat merged list gives the caller no reason to prefer the single call.
func grepBranches(pattern string) []string {
	if pattern == "" || !strings.Contains(pattern, "|") {
		if pattern == "" {
			return nil
		}
		return []string{pattern}
	}
	parts := strings.Split(pattern, "|")
	for _, p := range parts {
		p = strings.TrimSpace(p)
		// A branch that is not a bare word means the pattern is doing something
		// structural; report it whole rather than guessing at its parts.
		if p == "" || strings.ContainsAny(p, "()[]{}.*+?^$\\") {
			return []string{pattern}
		}
	}
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		out = append(out, strings.TrimSpace(p))
	}
	return out
}

// branchIndex returns the index of the first branch matching this shape, or
// len(branches) when none does (detail-only hits, or a single unsplit pattern).
func branchIndex(branches []string, s shape) int {
	for i, b := range branches {
		if strings.Contains(strings.ToLower(s.Name), strings.ToLower(b)) {
			return i
		}
	}
	return len(branches)
}
