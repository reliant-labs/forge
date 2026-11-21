package codemod

import (
	"regexp"
	"strings"
)

// The build half of tests-capture: the root Dockerfile's cover stages and
// .air.toml's build flags. Both are how an env that runs instrumented builds
// (the dev env and previews of a project with tests.coverage on) gets them;
// neither changes the production image or a bare `air`.

const (
	tcDockerfile = "Dockerfile"
	tcAirTOML    = ".air.toml"
)

var (
	dfFromRe      = regexp.MustCompile(`(?i)^FROM\s+(\S+)(?:\s+--\S+)*\s+AS\s+(\S+)\s*$`)
	dfCoverRe     = regexp.MustCompile(`(?im)^\s*FROM\s+\S+(\s+--\S+)*\s+AS\s+cover\s*$`)
	dfBuildOutRe  = regexp.MustCompile(`-o /app/bin/(\S+)`)
	dfPGOArgRe    = regexp.MustCompile(`^ARG FORGE_PGO_\w+=\S*$`)
	dfCopyBinRe   = regexp.MustCompile(`^COPY --from=builder /app/bin/(\S+) (\S+)$`)
	airCmdRe      = regexp.MustCompile(`^(\s*)cmd = (["'])(.*)(["'])\s*$`)
	airSectionRe  = regexp.MustCompile(`^\s*\[([^\]]+)\]\s*$`)
	airGoBuild    = "go build "
	airWinGoBuild = "; go build "
)

// dfInstr is one Dockerfile instruction: its lines (continuations joined in
// text, newline-separated) and its keyword.
type dfInstr struct {
	kw   string
	text string
}

// dfStage is one stage: its FROM line index, the index after its last line,
// and its instructions (comments dropped).
type dfStage struct {
	base, name string
	from, end  int
	instrs     []dfInstr
}

func dfStages(lines []string) []dfStage {
	var out []dfStage
	for i, l := range lines {
		if m := dfFromRe.FindStringSubmatch(strings.TrimRight(l, "\n")); m != nil {
			out = append(out, dfStage{base: m[1], name: strings.ToLower(m[2]), from: i})
		}
	}
	for n := range out {
		out[n].end = len(lines)
		if n+1 < len(out) {
			out[n].end = out[n+1].from
		}
		var cur []string
		for i := out[n].from + 1; i < out[n].end; i++ {
			l := strings.TrimRight(lines[i], "\n")
			if len(cur) == 0 && (strings.TrimSpace(l) == "" || strings.HasPrefix(strings.TrimSpace(l), "#")) {
				continue
			}
			cur = append(cur, l)
			if strings.HasSuffix(l, "\\") {
				continue
			}
			text := strings.Join(cur, "\n")
			kw := strings.ToUpper(strings.Fields(text)[0])
			out[n].instrs = append(out[n].instrs, dfInstr{kw: kw, text: text})
			cur = nil
		}
	}
	return out
}

func (r *tcRun) dockerfile() {
	src, ok := r.read(tcDockerfile)
	if !ok || dfCoverRe.MatchString(src) {
		return
	}
	lines := strings.SplitAfter(src, "\n")
	var builder, production *dfStage
	stages := dfStages(lines)
	for i := range stages {
		switch stages[i].name {
		case "builder":
			builder = &stages[i]
		case "production":
			production = &stages[i]
		}
	}
	unrecognised := func(why string) {
		r.next("%s: %s, so no cover stage was added and an env that instruments its Go workloads builds them uninstrumented. "+
			"Add the cover-builder and cover stages by hand: forge skill load migrations/v1.9.0/tests, section \"Instrumented builds\"", tcDockerfile, why)
	}
	if builder == nil || production == nil {
		unrecognised("it has no `builder` or no `production` stage")
		return
	}

	// cover-builder: each of the builder's binary builds again, instrumented.
	var cb strings.Builder
	cb.WriteString(tcBlock("dockerfile_cover_builder_comment"))
	cb.WriteString("FROM builder AS cover-builder\n\nARG FORGE_COVERPKG=\n")
	built := map[string]bool{}
	for i, in := range builder.instrs {
		if in.kw != "RUN" || !strings.Contains(in.text, airGoBuild) {
			continue
		}
		out := dfBuildOutRe.FindStringSubmatch(in.text)
		if out == nil || strings.Contains(in.text, " -cover") || strings.Count(in.text, airGoBuild) != 1 {
			continue
		}
		if i > 0 && builder.instrs[i-1].kw == "ARG" && dfPGOArgRe.MatchString(builder.instrs[i-1].text) {
			cb.WriteString(builder.instrs[i-1].text + "\n")
		}
		run := strings.Replace(in.text, airGoBuild, airGoBuild+"-cover -covermode=atomic ${FORGE_COVERPKG:+-coverpkg=${FORGE_COVERPKG}} ", 1)
		run = strings.Replace(run, "-o /app/bin/", "-o /app/cover/", 1)
		cb.WriteString(run + "\n")
		built[out[1]] = true
	}
	if len(built) == 0 {
		unrecognised("its builder stage has no `go build … -o /app/bin/<bin>` RUN")
		return
	}

	// cover: the production stage, its binaries swapped for the instrumented
	// ones, with GOCOVERDIR where Go writes the counters.
	var groups [][]string
	lastCopy := -1
	for _, in := range production.instrs {
		text := in.text
		if m := dfCopyBinRe.FindStringSubmatch(text); len(m) == 3 && built[m[1]] {
			text = "COPY --from=cover-builder /app/cover/" + m[1] + " " + m[2]
		}
		n := len(groups)
		join := n > 0 && ((in.kw == "COPY" && (groups[n-1][0] == "WORKDIR" || groups[n-1][0] == "COPY")) ||
			(in.kw == "CMD" && groups[n-1][0] == "ENTRYPOINT"))
		if join {
			groups[n-1] = append(groups[n-1], text)
		} else {
			groups = append(groups, []string{in.kw, text})
		}
		if in.kw == "COPY" {
			lastCopy = len(groups) - 1
		}
	}
	if lastCopy < 0 {
		unrecognised("its production stage copies no binary from the builder")
		return
	}
	var cs strings.Builder
	cs.WriteString(tcBlock("dockerfile_cover_comment"))
	cs.WriteString("FROM " + production.base + " AS cover\n")
	for n, g := range groups {
		cs.WriteString("\n" + strings.Join(g[1:], "\n") + "\n")
		if n == lastCopy {
			cs.WriteString("\nENV GOCOVERDIR=/tmp\n")
		}
	}

	// Before the production stage and the comment block that heads it.
	at := production.from
	for at > 0 && strings.HasPrefix(strings.TrimSpace(lines[at-1]), "#") {
		at--
	}
	stage := cb.String() + "\n" + cs.String() + "\n"
	if at > 0 && strings.TrimSpace(lines[at-1]) != "" {
		stage = "\n" + stage
	}
	next := strings.Join(lines[:at], "") + stage + strings.Join(lines[at:], "")
	r.set(tcDockerfile, next, "the cover-builder and cover stages (every binary built -cover -covermode=atomic; forge build uses them for an env that instruments, the production image is unchanged)")
}

func (r *tcRun) airTOML() {
	src, ok := r.read(tcAirTOML)
	if !ok || strings.Contains(src, "FORGE_GO_BUILD_FLAGS") {
		return
	}
	lines := strings.SplitAfter(src, "\n")
	section := ""
	done := map[string]bool{}
	for i := 0; i < len(lines); i++ {
		l := strings.TrimRight(lines[i], "\n")
		if m := airSectionRe.FindStringSubmatch(l); m != nil {
			section = m[1]
			continue
		}
		m := airCmdRe.FindStringSubmatch(l)
		if len(m) != 5 || m[2] != m[4] || done[section] {
			continue
		}
		cmd := m[3]
		var comment string
		switch section {
		case "build":
			if strings.Count(cmd, airGoBuild) != 1 {
				continue
			}
			cmd = strings.Replace(cmd, airGoBuild, airGoBuild+"$FORGE_GO_BUILD_FLAGS ", 1)
			comment = tcBlock("air_comment")
			if i == 0 || !strings.HasPrefix(strings.TrimSpace(lines[i-1]), "#") {
				comment = strings.TrimPrefix(comment, "  #\n")
			}
		case "build.windows":
			if strings.Count(cmd, airWinGoBuild) != 1 {
				continue
			}
			cmd = strings.Replace(cmd, airWinGoBuild, `; $f = @("$env:FORGE_GO_BUILD_FLAGS" -split " " | Where-Object { $_ }); go build @f `, 1)
			comment = tcBlock("air_windows_comment")
		default:
			continue
		}
		done[section] = true
		lines[i] = comment + m[1] + "cmd = " + m[2] + cmd + m[4] + "\n"
	}
	if !done["build"] {
		r.next("%s: [build] cmd is not one `go build` command, so it was left as is and the dev server builds uninstrumented. "+
			"Pass $FORGE_GO_BUILD_FLAGS to its go build (forge skill load migrations/v1.9.0/tests, section \"Instrumented builds\")", tcAirTOML)
		return
	}
	r.set(tcAirTOML, strings.Join(lines, ""), "go build reads $FORGE_GO_BUILD_FLAGS (forge env up sets it to -cover … for a dev env that instruments; unset, nothing changes)")
}
