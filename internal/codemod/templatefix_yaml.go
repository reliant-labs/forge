package codemod

import (
	"regexp"
	"strings"
)

// templatefix_yaml.go — the template fixes to the Tier-2 YAML starters
// (Taskfile.yml, .golangci.yml). `forge project upgrade` replaces an unedited
// one whole; these carry the one fix into a copy the project has edited.

func init() {
	registerTemplateFix(templateFix{
		name:    "golangci-append-assign",
		version: "v1.12.0",
		short:   "Stop .golangci.yml flagging the append(buf[:0], src...) buffer-reuse idiom as gocritic appendAssign (forge#2920)",
		long: `Add the scaffolded .golangci.yml's appendAssign exclusion to an edited copy.

gocritic's appendAssign finds 'a = append(b, x)' typos, and its one false
positive is the buffer-reuse copy 'dst = append(buf[:0], src...)'. Since
v1.12.0 the scaffolded config excludes exactly that shape (a [:0] base and a
single spread argument), scoped to the check and the line, never a path.

This adds the rule after the '- path: _test\.go' exclusion rule. A config whose
exclusions already name appendAssign is left alone; one with no '- path: _test\.go'
rule under linters.exclusions.rules is listed with the rule to add.`,
		candidates: globCandidates(".golangci.yml"),
		tier2:      true,
		rewrite:    rewriteGolangciAppendAssign,
	})
	registerTemplateFix(templateFix{
		name:    "taskfile-short-skip-summary",
		version: "v1.12.0",
		short:   "Make task test:short report the tests testkit.Slow skipped, through forge test capture --no-record (forge#2922)",
		long: `Route an edited Taskfile.yml's test:short through forge test capture.

'go test -short' shows a package whose testkit.Slow tests all skipped as the
same green 'ok' as one where they ran. Since v1.12.0 the scaffolded test:short
runs 'go test -short -json' under 'forge test capture --no-record', which prints
what go test prints, exits with its status, records nothing, and ends with a
line counting the skipped tests.

This replaces test:short's command when it is still the template's
'go test -short -timeout 60s ...' line, and adds the CAPTURE var it reads. A
Taskfile with no top-level FORGE var (one older than the pinned-forge
Taskfile), or whose test:short command was edited, is listed, not rewritten.`,
		candidates: globCandidates("Taskfile.yml"),
		tier2:      true,
		rewrite:    rewriteTaskfileShortSkipSummary,
	})
}

// golangciAppendAssignRule is the exclusion rule golangci.yml.tmpl writes
// under linters.exclusions.rules, at six spaces of indent.
const golangciAppendAssignRule = `      # appendAssign finds ` + "`a = append(b, x)`" + ` typos, so it stays on. Its one
      # false positive is the buffer-reuse copy — ` + "`dst = append(buf[:0],\n      # src...)`" + `, copying a whole slice into another slice's backing array on
      # purpose. Only that exact shape is excluded: a ` + "`[:0]`" + ` base AND a single
      # spread argument. ` + "`a = append(b[:0], x)`" + ` is still reported. Scoped to
      # the check and the line's shape, never to a path.
      - linters:
          - gocritic
        text: "appendAssign"
        source: 'append\(\s*[^,()]+\[:0\]\s*,\s*[^,()]+\.\.\.\s*\)'
`

// golangciExcludesAppendAssign matches an exclusion rule already naming the
// check (a comment that mentions it does not count).
var golangciExcludesAppendAssign = regexp.MustCompile(`(?m)^\s*text:\s*["']?appendAssign`)

var golangciTestRule = regexp.MustCompile(`(?m)^(\s*)- path: _test\\\.go\s*$`)

func rewriteGolangciAppendAssign(_ string, src []byte) (out []byte, note, next string) {
	s := string(src)
	if golangciExcludesAppendAssign.MatchString(s) {
		return nil, "", ""
	}
	if !strings.Contains(s, "gocritic") {
		// gocritic is not enabled here: nothing to exclude.
		return nil, "", ""
	}
	loc := golangciTestRule.FindStringSubmatchIndex(s)
	if loc == nil {
		return nil, "", "no `- path: _test\\.go` rule under linters.exclusions.rules to place it after; add this rule there:\n" + golangciAppendAssignRule
	}
	indent := s[loc[2]:loc[3]]
	indent = indent[strings.LastIndexByte(indent, '\n')+1:]
	// The rule's item runs to the next line indented no deeper than its
	// "- " (a sibling rule, or the end of the list); the new rule goes
	// after its last non-blank line.
	at := loc[1]
	if at < len(s) && s[at] == '\n' {
		at++
	}
	for cur := at; cur < len(s); {
		end := strings.IndexByte(s[cur:], '\n')
		line := s[cur:]
		if end >= 0 {
			line = s[cur : cur+end]
		}
		trimmed := strings.TrimLeft(line, " ")
		if trimmed != "" && len(line)-len(trimmed) <= len(indent) {
			break
		}
		if end < 0 {
			cur = len(s)
		} else {
			cur += end + 1
		}
		if trimmed != "" {
			at = cur
		}
	}
	rule := golangciAppendAssignRule
	if indent != "      " {
		rule = reindent(rule, "      ", indent)
	}
	if at == len(s) && !strings.HasSuffix(s, "\n") {
		s += "\n"
		at = len(s)
	}
	return []byte(s[:at] + rule + s[at:]), "added the appendAssign exclusion for append(buf[:0], src...) buffer reuse", ""
}

// reindent swaps each line's leading from for to.
func reindent(block, from, to string) string {
	lines := strings.SplitAfter(block, "\n")
	for i, l := range lines {
		if strings.HasPrefix(l, from) {
			lines[i] = to + strings.TrimPrefix(l, from)
		}
	}
	return strings.Join(lines, "")
}

// The test:short command Taskfile.yml.tmpl wrote before v1.12.0, and what
// replaces it (the CAPTURE var and the capture command).
const (
	taskfileShortOld = "    cmds:\n      - go test -short -timeout 60s {{if .CLI_ARGS}}{{.CLI_ARGS}}{{else}}./...{{end}}\n"
	taskfileShortNew = "    vars:\n" +
		"      # CAPTURE is the forge that prints this run: FORGE, unless FORGE is a\n" +
		"      # `go run` of the pin (about a second per call, even cached) and the\n" +
		"      # forge on PATH has `test capture --no-record` (a tenth of that). `test\n" +
		"      # capture` is the same plumbing in every forge that has it and never\n" +
		"      # hands off to the pin, so which one prints the run changes nothing but\n" +
		"      # what the inner loop waits for.\n" +
		"      CAPTURE:\n" +
		`        sh: 'case "{{.FORGE}}" in "go run "*) forge test capture --no-record --help >/dev/null 2>&1 && echo forge || echo "{{.FORGE}}";; *) echo "{{.FORGE}}";; esac'` + "\n" +
		"    cmds:\n" +
		`      - '{{.CAPTURE}} test capture --no-record{{if contains "-json" .CLI_ARGS}} --render raw{{end}} -- go test -short -json -timeout 60s {{if .CLI_ARGS}}{{.CLI_ARGS}}{{else}}./...{{end}}'` + "\n"
)

var taskfileForgeVar = regexp.MustCompile(`(?m)^  FORGE:`)

func rewriteTaskfileShortSkipSummary(_ string, src []byte) (out []byte, note, next string) {
	s := string(src)
	i := strings.Index(s, "\n  test:short:\n")
	if i < 0 {
		return nil, "", ""
	}
	// The task's body runs to the next task (two-space indent).
	body := s[i+len("\n  test:short:\n"):]
	if j := regexp.MustCompile(`(?m)^  \S`).FindStringIndex(body); j != nil {
		body = body[:j[0]]
	}
	if !strings.Contains(body, taskfileShortOld) {
		return nil, "", ""
	}
	if !taskfileForgeVar.MatchString(s) {
		return nil, "", "test:short would run `{{.FORGE}} test capture`, but this Taskfile declares no top-level FORGE var; " +
			"run `forge project upgrade --check Taskfile.yml` to see the whole template diff"
	}
	start := i + len("\n  test:short:\n")
	newBody := strings.Replace(body, taskfileShortOld, taskfileShortNew, 1)
	return []byte(s[:start] + newBody + s[start+len(body):]),
		"test:short runs under `forge test capture --no-record`, which counts the tests testkit.Slow skipped", ""
}
