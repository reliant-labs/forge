package affected

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"

	"github.com/reliant-labs/forge/internal/config"
	"github.com/reliant-labs/forge/internal/goexec"
)

// Options is one `forge ci affected` invocation. Both the forge verb and
// cmd/affected (the thin binary forge's own Taskfile builds in a second, not
// the 1,600-package CLI) fill it and call Run.
type Options struct {
	Root string
	Gate config.CIGateConfig
	// Base overrides the ref the diff is measured from (default CI_BASE, then
	// origin/<default branch>); Files judges a list instead of the work tree.
	Base, Files string
	// Lane is unit (default), e2e or integration.
	Lane string
	// Packages prints only the package list on stdout (the plan goes to
	// stderr); JSON prints the whole decision. Full forces the whole module.
	Packages, JSON, Full bool
}

// Run decides and prints. An unknown lane is the only error: every other
// failure to decide is a full-module answer with its reason.
func Run(ctx context.Context, o Options, stdout, stderr io.Writer) error {
	lane := o.Lane
	if lane == "" {
		lane = "unit"
	}
	if lane != "unit" && lane != "e2e" && lane != "integration" {
		return fmt.Errorf("unknown lane %q: use unit, e2e or integration", lane)
	}
	base := o.Base
	if base == "" {
		base = os.Getenv(BaseEnv)
	}
	force := ""
	if o.Full || envTruthy(os.Getenv(FullEnv)) {
		force = "--full / " + FullEnv + "=1"
	}
	in := Input{
		Root: o.Root, BaseRef: base, Env: goexec.Env(), Force: force,
		Ignore: o.Gate.AffectedIgnore, Rules: o.Gate.AffectedRules,
	}
	if o.Files != "" {
		list, err := fileList(o.Files)
		if err != nil {
			return fmt.Errorf("--files: %w", err)
		}
		in.Files = list
	}
	res := Compute(ctx, in)

	pkgs := res.Packages
	if lane != "unit" {
		pkgs = res.Tagged[lane]
	}
	switch {
	case o.JSON:
		enc := json.NewEncoder(stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(struct {
			Lane     string   `json:"lane"`
			Result   Result   `json:"result"`
			Selected []string `json:"selected"`
		}{lane, res, pkgs})
	case o.Packages:
		res.WritePlan(stderr, lane, pkgs)
		if res.Full {
			_, err := fmt.Fprintln(stdout, "./...")
			return err
		}
		sorted := append([]string(nil), pkgs...)
		sort.Strings(sorted)
		_, err := fmt.Fprintln(stdout, strings.Join(sorted, " "))
		return err
	default:
		res.WritePlan(stdout, lane, pkgs)
		return nil
	}
}

func envTruthy(v string) bool {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "1", "true", "yes", "on":
		return true
	}
	return false
}

func fileList(spec string) ([]string, error) {
	if path, ok := strings.CutPrefix(spec, "@"); ok {
		data, err := os.ReadFile(path)
		if err != nil {
			return nil, err
		}
		return strings.Fields(string(data)), nil
	}
	return strings.Split(spec, ","), nil
}
