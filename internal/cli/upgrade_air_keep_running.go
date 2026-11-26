package cli

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"reflect"
	"regexp"
	"sort"
	"strings"

	"github.com/BurntSushi/toml"

	"github.com/reliant-labs/forge/internal/codemod"
	"github.com/reliant-labs/forge/internal/config"
	"github.com/reliant-labs/forge/internal/templates"
)

// `forge fix air-keep-running`: the mechanical half of the playbook
// migrations/v1.12.0/air-keep-running.
//
// air restarts the binary it runs after every build, a failed one included,
// so with the scaffolded stop_on_error = false a compile error used to cost a
// full server restart onto the OLD binary (forge friction log, Ranger item
// #2). From v1.12.0 the scaffolded air configs build through `forge dev
// air-build` (internal/airbuild), which never returns from a failed build
// while air runs, so air never restarts the server for it and the server keeps
// serving the last good build. .air.toml and .air-debug.toml are scaffold-once, so moving the
// pin changes neither: this codemod is what moves them.
//
// It rewrites a [build] cmd only when it is byte for byte a form forge
// scaffolded (airKeepRunningOldRe: with and without $FORGE_GO_BUILD_FLAGS,
// and the debug config's, with and without -trimpath=false) for one binary
// name, into the current template's form for that name. That form runs the
// same `go build`, writes the same ./tmp/<name>.tmp, and leaves forge, not
// `mv`, to install it. The leading `forge storage check ./tmp` stays as it
// is. [build.windows] is never touched: air stops the binary before it builds
// on Windows, so there is no server to keep. Any other [build] cmd is reported as the edit to make by hand. The result must decode
// as TOML to the old document with only that cmd changed.
//
// forge.yaml must pin v1.12.0 or later: a bare `air` (task dev) runs the
// forge on PATH, which the pin decides, and an older one has no `dev
// air-build`.

// airKeepRunningRelease is the release the codemod, its playbook and the
// template's ForgeAtLeast guard ship in.
const airKeepRunningRelease = "v1.12.0"

// airKeepRunningPlaybookID is the playbook's ID, as `forge project upgrade
// list` prints it.
const airKeepRunningPlaybookID = airKeepRunningRelease + "/air-keep-running"

// airKeepRunningMarker is what a [build] cmd that already holds failed builds
// runs.
const airKeepRunningMarker = "forge dev air-build"

// airKeepRunningOldRe is a [build] cmd exactly as an older forge scaffolded
// it: the flags (group 1) of .air.toml (with or without
// $FORGE_GO_BUILD_FLAGS) or .air-debug.toml (with or without
// -trimpath=false), and the binary name four times (groups 2-5).
var airKeepRunningOldRe = regexp.MustCompile(`^forge storage check \./tmp && CGO_ENABLED=0 go build ` +
	`((?:\$FORGE_GO_BUILD_FLAGS )?-buildvcs=false |-gcflags='all=-N -l' (?:-trimpath=false )?-buildvcs=false )` +
	`-o \./tmp/([A-Za-z0-9_.\-]+)\.tmp \./cmd/([A-Za-z0-9_.\-]+) && mv \./tmp/([A-Za-z0-9_.\-]+)\.tmp \./tmp/([A-Za-z0-9_.\-]+)$`)

func init() {
	codemod.Register(codemod.Codemod{
		Name:    "air-keep-running",
		Version: airKeepRunningRelease,
		Short:   "Make a failed air rebuild leave the running server alone instead of restarting the old binary",
		Long: `Rewrite the [build] cmd of .air.toml and .air-debug.toml (and any air_config
an OnHost declares) to build through ` + "`forge dev air-build`" + `, as forge
scaffolds them from ` + airKeepRunningRelease + `. air restarts its binary whenever a build
returns, a failed one included; ` + "`forge dev air-build`" + ` holds a failed build until
air exits, so air never restarts the server for it and the server keeps
serving the last good build. The next successful build restarts it once.

Only a command that is byte for byte what an older forge scaffolded is
rewritten; the rest of the file, [build.windows] and the leading
` + "`forge storage check ./tmp`" + ` are left as they are. An edited command is reported
with the edit to make by hand. forge.yaml must pin ` + airKeepRunningRelease + ` or later, since a
bare ` + "`air`" + ` runs the forge on PATH: ` + "`forge project upgrade --to " + airKeepRunningRelease + "`" + ` moves the pin
and then runs this. Re-running it changes nothing.
The rest of the story: ` + "`forge skill load migrations/" + airKeepRunningPlaybookID + "`" + `.`,
		Run: runAirKeepRunning,
	})
}

// projectNeedsAirKeepRunning is the playbook's detection (`forge-detect
// air-keep-running`): whether the codemod would change anything, or report a
// manual step.
func projectNeedsAirKeepRunning(dir string) bool {
	rep, err := runAirKeepRunning(dir, codemod.Options{})
	return err == nil && (rep.Changed() || len(rep.Next) > 0)
}

func runAirKeepRunning(dir string, opts codemod.Options) (codemod.Report, error) {
	var rep codemod.Report
	configs, err := airKeepRunningConfigs(dir)
	if err != nil {
		return rep, err
	}
	type edit struct {
		path string
		out  []byte
	}
	var edits []edit
	for _, rel := range configs {
		p := filepath.Join(dir, filepath.FromSlash(rel))
		b, err := os.ReadFile(p) //nolint:gosec // the project's own air config
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return rep, err
		}
		out, rewrites, next := planAirKeepRunning(rel, string(b))
		rep.Next = append(rep.Next, next...)
		if out != string(b) {
			edits = append(edits, edit{p, []byte(out)})
			rep.Rewrites = append(rep.Rewrites, rewrites...)
		}
	}
	if len(edits) == 0 {
		return rep, nil
	}
	cfg, err := config.LoadProjectDir(dir)
	if err != nil {
		rep.Rewrites = nil
		rep.Next = append(rep.Next, fmt.Sprintf("forge.yaml did not load (%v): fix it, then re-run `%s fix air-keep-running`", err, Name()))
		return rep, nil
	}
	if !templates.ForgeAtLeast(cfg.ForgeVersion, airKeepRunningRelease) {
		rep.Rewrites = nil
		rep.Next = append(rep.Next, fmt.Sprintf("forge.yaml pins forge %q, which has no `forge dev air-build` (it ships in %s) and is the forge a bare `air` runs: `%s project upgrade --to %s` (or later) moves the pin and then runs this",
			cfg.ForgeVersion, airKeepRunningRelease, Name(), airKeepRunningRelease))
		return rep, nil
	}
	if opts.Write {
		for _, e := range edits {
			if err := writeKeepingMode(e.path, e.out); err != nil {
				return rep, err
			}
		}
	}
	rep.Verify = append(rep.Verify, Name()+" env up dev, break a .go file and save: the server's PID stays the same and it keeps serving; fix it and save: one restart")
	return rep, nil
}

// airTemplateData renders a scaffolded air config outside the generator: the
// binary name, and the pin its ForgeAtLeast guards read ("" renders every
// guarded block's older form).
type airTemplateData struct{ Name, ForgeVersion string }

// ForgeAtLeast reports whether ForgeVersion has everything release has.
func (d airTemplateData) ForgeAtLeast(release string) bool {
	return templates.ForgeAtLeast(d.ForgeVersion, release)
}

// airKeepRunningConfigs are the air configs to read, project-relative:
// .air.toml, .air-debug.toml and each air_config the KCL names.
func airKeepRunningConfigs(dir string) ([]string, error) {
	seen := map[string]bool{".air.toml": true, ".air-debug.toml": true}
	files, err := trimpathKCLFiles(dir)
	if err != nil {
		return nil, err
	}
	for _, rel := range files {
		b, err := os.ReadFile(filepath.Join(dir, filepath.FromSlash(rel))) //nolint:gosec // the project's own KCL
		if err != nil {
			return nil, err
		}
		for _, m := range kclAirConfigRe.FindAllStringSubmatch(string(b), -1) {
			p := path.Clean(filepath.ToSlash(m[1]))
			if path.IsAbs(p) || p == ".." || strings.HasPrefix(p, "../") {
				continue
			}
			seen[p] = true
		}
	}
	out := make([]string, 0, len(seen))
	for p := range seen {
		out = append(out, p)
	}
	sort.Strings(out)
	return out, nil
}

// planAirKeepRunning is one air config's edit: the new content (src when
// nothing changes), the report lines, and the manual steps.
func planAirKeepRunning(rel, src string) (string, []string, []string) {
	var before map[string]any
	if _, err := toml.Decode(src, &before); err != nil {
		if strings.Contains(src, "go build") && !strings.Contains(src, airKeepRunningMarker) {
			return src, nil, []string{fmt.Sprintf("%s does not parse as TOML (%v), so it was left alone. %s", rel, err, airKeepRunningByHand)}
		}
		return src, nil, nil
	}
	build, _ := before["build"].(map[string]any)
	value, _ := build["cmd"].(string)
	if value == "" || strings.Contains(value, airKeepRunningMarker) {
		return src, nil, nil
	}
	lines := strings.SplitAfter(src, "\n")
	section := ""
	for i, line := range lines {
		body := strings.TrimRight(line, "\r\n")
		if m := airSectionRe.FindStringSubmatch(body); m != nil {
			section = strings.Join(strings.Fields(m[1]), "")
			continue
		}
		if section != "build" {
			continue
		}
		m := airCmdRe.FindStringSubmatch(body)
		if len(m) != 6 || m[2] != m[4] {
			continue
		}
		cur, ok := airKeepRunningCmd(m[3])
		if !ok || m[3] != value {
			return src, nil, []string{fmt.Sprintf("%s:%d: [build] cmd is not a form forge scaffolded, so it was left alone. %s", rel, i+1, airKeepRunningByHand)}
		}
		lines[i] = m[1] + m[2] + cur + m[4] + m[5] + line[len(body):]
		out := strings.Join(lines, "")
		if why := verifyAirKeepRunning(before, out); why != "" {
			return src, nil, []string{fmt.Sprintf("%s: forge's edit did not verify (%s), so it was left alone. %s", rel, why, airKeepRunningByHand)}
		}
		return out, []string{fmt.Sprintf("%s:%d: [build] cmd builds through `forge dev air-build`, so a failed build leaves the running server alone", rel, i+1)}, nil
	}
	return src, nil, []string{fmt.Sprintf("%s: [build] cmd is not a form forge scaffolded, so it was left alone. %s", rel, airKeepRunningByHand)}
}

// airKeepRunningByHand is the manual step for a [build] cmd forge does not
// rewrite.
const airKeepRunningByHand = "To keep the running server through a failed build, run its `go build` as " +
	"`forge dev air-build --out <the binary air runs> -- go build … -o <that binary>.tmp …` and drop the `&& mv` " +
	"that installed it (forge installs it). If you precheck storage, pass its exit status with `--precheck-status $?`; see migrations/" + airKeepRunningPlaybookID

// airKeepRunningCmd is the current template's form of value, a [build] cmd
// in an older forge's form; false when value is not such a form.
func airKeepRunningCmd(value string) (string, bool) {
	m := airKeepRunningOldRe.FindStringSubmatch(value)
	if len(m) != 6 || m[2] != m[3] || m[2] != m[4] || m[2] != m[5] {
		return "", false
	}
	name, flags := m[2], m[1]
	return "forge storage check ./tmp; CGO_ENABLED=0 forge dev air-build --precheck-status $? --out ./tmp/" + name + " -- go build " + flags +
		"-o ./tmp/" + name + ".tmp ./cmd/" + name, true
}

// verifyAirKeepRunning proves an edit: the result decodes, its [build] cmd
// runs `forge dev air-build`, and nothing else decodes differently.
func verifyAirKeepRunning(before map[string]any, out string) string {
	var after map[string]any
	if _, err := toml.Decode(out, &after); err != nil {
		return fmt.Sprintf("the result does not parse: %v", err)
	}
	build, _ := after["build"].(map[string]any)
	if cmd, _ := build["cmd"].(string); !strings.Contains(cmd, airKeepRunningMarker) {
		return "[build] cmd does not run `" + airKeepRunningMarker + "` afterwards"
	}
	for _, doc := range []map[string]any{before, after} {
		if b, _ := doc["build"].(map[string]any); b != nil {
			delete(b, "cmd")
		}
	}
	if !reflect.DeepEqual(before, after) {
		return "the rest of the file decodes differently afterwards"
	}
	return ""
}
