package imagebuild

// A Dockerfile interpreter for the instructions that need no code execution.
//
// A forge-scaffolded Dockerfile's FINAL stage is
//
//	FROM alpine:3.21
//	RUN apk --no-cache add ca-certificates
//	WORKDIR /app
//	COPY --from=builder /app/bin/app ./app
//	USER 65532:65532
//	EXPOSE 8080
//	HEALTHCHECK ... CMD wget ...
//	ENTRYPOINT ["/app/app"]
//	CMD ["server"]
//
// Everything in it except the RUN is metadata or a file copy, and forge has
// already compiled the binaries the COPYs name. So the image can be assembled
// by appending layers to the base image instead of running a build daemon.
//
// The interpreter is STRICT. An instruction it does not model is an error that
// names the line, never a skip: skipping a RUN would ship an image that differs
// from the one `docker build` makes, and nobody would be told. The single
// exception is `RUN apk add ca-certificates`, which is checked against the base
// image rather than ignored (see opRequireFile).

import (
	"encoding/json"
	"errors"
	"fmt"
	"path"
	"strconv"
	"strings"
	"time"

	v1 "github.com/google/go-containerregistry/pkg/v1"
)

// UnsupportedError is a Dockerfile construct the in-process builder does not
// model. It carries the line so the message points at the file.
type UnsupportedError struct {
	Line  int
	Stage string
	What  string
}

func (e *UnsupportedError) Error() string {
	where := fmt.Sprintf("Dockerfile line %d", e.Line)
	if e.Stage != "" {
		where += fmt.Sprintf(" (stage %q)", e.Stage)
	}
	return where + ": " + e.What
}

type instruction struct {
	cmd   string
	flags []string // leading --name=value words
	args  string   // everything after the command and flags, continuation-joined
	line  int
}

type stage struct {
	name  string // AS name, "" when unnamed
	index int
	base  string // raw FROM image (unexpanded)
	line  int
	insts []instruction
}

type dockerfile struct {
	globalArgs []instruction // ARGs before the first FROM
	stages     []*stage
}

// parseDockerfile splits src into stages of instructions. It joins
// continuation lines, drops comments and blank lines, and rejects heredocs.
func parseDockerfile(src []byte) (*dockerfile, error) {
	text := strings.ReplaceAll(string(src), "\r\n", "\n")
	text = strings.TrimPrefix(text, "\ufeff")
	lines := strings.Split(text, "\n")

	df := &dockerfile{}
	var cur *stage
	var pending strings.Builder
	pendingLine := 0

	flush := func() error {
		raw := strings.TrimSpace(pending.String())
		pending.Reset()
		if raw == "" {
			return nil
		}
		inst, err := splitInstruction(raw, pendingLine)
		if err != nil {
			return err
		}
		if inst.cmd == "FROM" {
			st, err := parseFrom(inst, len(df.stages))
			if err != nil {
				return err
			}
			cur = st
			df.stages = append(df.stages, st)
			return nil
		}
		if cur == nil {
			if inst.cmd != "ARG" {
				return &UnsupportedError{Line: inst.line, What: inst.cmd + " before the first FROM"}
			}
			df.globalArgs = append(df.globalArgs, inst)
			return nil
		}
		cur.insts = append(cur.insts, inst)
		return nil
	}

	continuing := false
	for i, line := range lines {
		trimmed := strings.TrimSpace(line)
		if !continuing && (trimmed == "" || strings.HasPrefix(trimmed, "#")) {
			continue
		}
		// BuildKit allows comment and blank lines inside a continuation.
		if continuing && (trimmed == "" || strings.HasPrefix(trimmed, "#")) {
			continue
		}
		if !continuing {
			pendingLine = i + 1
		}
		if strings.HasSuffix(trimmed, "\\") {
			pending.WriteString(strings.TrimSuffix(trimmed, "\\"))
			pending.WriteByte(' ')
			continuing = true
			continue
		}
		pending.WriteString(trimmed)
		continuing = false
		if err := flush(); err != nil {
			return nil, err
		}
	}
	if continuing {
		if err := flush(); err != nil {
			return nil, err
		}
	}
	if len(df.stages) == 0 {
		return nil, errors.New("the Dockerfile has no FROM")
	}
	return df, nil
}

func splitInstruction(raw string, line int) (instruction, error) {
	cmd, rest := raw, ""
	if i := strings.IndexAny(raw, " \t"); i >= 0 {
		cmd, rest = raw[:i], strings.TrimSpace(raw[i+1:])
	}
	inst := instruction{cmd: strings.ToUpper(cmd), line: line}
	for strings.HasPrefix(rest, "--") {
		flag, tail := rest, ""
		if i := strings.IndexAny(rest, " \t"); i >= 0 {
			flag, tail = rest[:i], strings.TrimSpace(rest[i+1:])
		}
		inst.flags = append(inst.flags, flag)
		rest = tail
	}
	inst.args = rest
	if hasHeredoc(inst.args) {
		return inst, &UnsupportedError{Line: line, What: inst.cmd + " uses a heredoc (<<)"}
	}
	return inst, nil
}

// hasHeredoc reports a `<<WORD` / `<<-WORD` / `<<"WORD"` token.
func hasHeredoc(args string) bool {
	for i := 0; i+2 < len(args); i++ {
		if args[i] == '<' && args[i+1] == '<' && (i == 0 || args[i-1] == ' ') {
			c := args[i+2]
			if c == '-' || c == '"' || c == '\'' || c == '_' || (c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z') {
				return true
			}
		}
	}
	return false
}

func parseFrom(inst instruction, index int) (*stage, error) {
	words := strings.Fields(inst.args)
	for _, f := range inst.flags {
		if !strings.HasPrefix(f, "--platform=") {
			return nil, &UnsupportedError{Line: inst.line, What: "FROM flag " + f}
		}
	}
	switch {
	case len(words) == 1:
		return &stage{index: index, base: words[0], line: inst.line}, nil
	case len(words) == 3 && strings.EqualFold(words[1], "AS"):
		return &stage{index: index, base: words[0], name: words[2], line: inst.line}, nil
	}
	return nil, &UnsupportedError{Line: inst.line, What: "malformed FROM " + inst.args}
}

// selectStage picks the stage `docker build` would build: --target when named,
// else the last one.
func (df *dockerfile) selectStage(target string) (*stage, error) {
	if target == "" {
		return df.stages[len(df.stages)-1], nil
	}
	for _, st := range df.stages {
		if st.name == target {
			return st, nil
		}
	}
	return nil, fmt.Errorf("the Dockerfile has no stage named %q", target)
}

func (df *dockerfile) stageNamed(name string) *stage {
	for _, st := range df.stages {
		if st.name == name {
			return st
		}
	}
	return nil
}

// globalVars resolves the ARGs declared before the first FROM, applying
// overrides from buildArgs.
func (df *dockerfile) globalVars(buildArgs map[string]string) map[string]string {
	out := map[string]string{}
	for _, inst := range df.globalArgs {
		for _, w := range strings.Fields(inst.args) {
			name, def, hasDef := strings.Cut(w, "=")
			if v, ok := buildArgs[name]; ok {
				out[name] = v
			} else if hasDef {
				out[name] = expandVars(strings.Trim(def, `"'`), out)
			}
		}
	}
	return out
}

// layerOpKind is the kind of filesystem change an instruction makes.
type layerOpKind int

const (
	opCopy layerOpKind = iota + 1
	// opMkdir is WORKDIR creating its directory.
	opMkdir
	// opRequireFile is `RUN apk add ca-certificates`: not executed, but the
	// base image must already hold the file the package would install.
	opRequireFile
	// opMeta is an instruction that changes only the image config (USER,
	// ENTRYPOINT, ...). It adds an empty-layer history entry, as docker does.
	opMeta
)

type layerOp struct {
	kind    layerOpKind
	line    int
	history string

	from string   // opCopy: "" for the build context, else a stage name
	srcs []string // opCopy
	dest string   // opCopy: absolute; a trailing slash means "directory"

	dirs []string // opMkdir
	path string   // opRequireFile
}

// evaluated is a stage reduced to what assembling it needs.
type evaluated struct {
	ops    []layerOp
	config v1.Config
	notes  []string
}

// stageEval is the running state of one stage's evaluation: the config being
// built, the variable scope and the ops emitted so far. Each instruction is a
// method, so the dispatch table stays flat.
type stageEval struct {
	df     *dockerfile
	st     *stage
	cfg    v1.Config
	ev     *evaluated
	env    map[string]string
	args   map[string]string
	global map[string]string
	build  map[string]string

	workdir string
	cmdSet  bool
}

// evaluateStage applies st's instructions over baseCfg (the base image's
// config, whose ENV takes part in variable expansion exactly as it does in
// docker). buildArgs override ARG defaults; global holds the pre-FROM ARGs.
func evaluateStage(df *dockerfile, st *stage, baseCfg v1.Config, buildArgs, global map[string]string) (*evaluated, error) {
	e := &stageEval{
		df: df, st: st, cfg: *baseCfg.DeepCopy(), ev: &evaluated{},
		args: map[string]string{}, global: global, build: buildArgs,
		workdir: baseCfg.WorkingDir,
	}
	e.env = envMap(e.cfg.Env)
	if e.workdir == "" {
		e.workdir = "/"
	}
	for _, inst := range st.insts {
		if err := e.apply(inst); err != nil {
			return nil, err
		}
	}
	e.ev.config = e.cfg
	return e.ev, nil
}

func (e *stageEval) vars() map[string]string {
	out := make(map[string]string, len(e.args)+len(e.env))
	for k, v := range e.args {
		out[k] = v
	}
	for k, v := range e.env {
		out[k] = v
	}
	return out
}

func (e *stageEval) unsupported(inst instruction, what string) error {
	return &UnsupportedError{Line: inst.line, Stage: e.st.name, What: what}
}

func (e *stageEval) noFlags(inst instruction) error {
	if len(inst.flags) > 0 {
		return e.unsupported(inst, inst.cmd+" flag "+inst.flags[0])
	}
	return nil
}

// words expands and splits inst's arguments.
func (e *stageEval) words(inst instruction) ([]string, error) {
	w, err := splitWords(inst.args, e.vars())
	if err != nil {
		return nil, e.unsupported(inst, err.Error())
	}
	return w, nil
}

// one is words for an instruction that takes exactly one argument.
func (e *stageEval) one(inst instruction) (string, error) {
	if err := e.noFlags(inst); err != nil {
		return "", err
	}
	w, err := e.words(inst)
	if err != nil || len(w) != 1 {
		return "", e.unsupported(inst, inst.cmd+" takes one argument")
	}
	return w[0], nil
}

// meta records an instruction that changes only the config, as the
// empty-layer history entry docker writes for it.
func (e *stageEval) meta(inst instruction) {
	h := strings.Join(strings.Fields(inst.cmd+" "+strings.Join(inst.flags, " ")+" "+inst.args), " ")
	e.ev.ops = append(e.ev.ops, layerOp{kind: opMeta, line: inst.line, history: h})
}

func (e *stageEval) apply(inst instruction) error {
	switch inst.cmd {
	case "ARG":
		return e.doARG(inst)
	case "ENV", "LABEL":
		return e.doKeyValues(inst)
	case "WORKDIR":
		return e.doWORKDIR(inst)
	case "USER":
		u, err := e.one(inst)
		if err != nil {
			return err
		}
		e.cfg.User = u
		e.meta(inst)
	case "STOPSIGNAL":
		sig, err := e.one(inst)
		if err != nil {
			return err
		}
		e.cfg.StopSignal = sig
		e.meta(inst)
	case "EXPOSE":
		return e.doEXPOSE(inst)
	case "ENTRYPOINT", "CMD":
		return e.doEntry(inst)
	case "HEALTHCHECK":
		hc, err := parseHealthcheck(inst)
		if err != nil {
			return e.unsupported(inst, err.Error())
		}
		e.cfg.Healthcheck = hc
		e.meta(inst)
	case "COPY":
		return e.doCOPY(inst)
	case "RUN":
		return e.doRUN(inst)
	default:
		return e.unsupported(inst, inst.cmd+" is not supported")
	}
	return nil
}

func (e *stageEval) doARG(inst instruction) error {
	if err := e.noFlags(inst); err != nil {
		return err
	}
	for _, w := range strings.Fields(inst.args) {
		name, def, hasDef := strings.Cut(w, "=")
		switch v, ok := e.build[name]; {
		case ok:
			e.args[name] = v
		case hasDef:
			e.args[name] = expandVars(strings.Trim(def, `"'`), e.vars())
		default:
			if g, ok := e.global[name]; ok {
				e.args[name] = g
			}
		}
	}
	return nil
}

func (e *stageEval) doKeyValues(inst instruction) error {
	if err := e.noFlags(inst); err != nil {
		return err
	}
	pairs, err := keyValues(inst, e.vars())
	if err != nil {
		return e.unsupported(inst, err.Error())
	}
	for _, kv := range pairs {
		if inst.cmd == "ENV" {
			e.env[kv[0]] = kv[1]
			e.cfg.Env = setEnv(e.cfg.Env, kv[0], kv[1])
			continue
		}
		if e.cfg.Labels == nil {
			e.cfg.Labels = map[string]string{}
		}
		e.cfg.Labels[kv[0]] = kv[1]
	}
	e.meta(inst)
	return nil
}

func (e *stageEval) doWORKDIR(inst instruction) error {
	dir, err := e.one(inst)
	if err != nil {
		return err
	}
	e.workdir = path.Clean(joinAbs(e.workdir, dir))
	e.cfg.WorkingDir = e.workdir
	e.ev.ops = append(e.ev.ops, layerOp{kind: opMkdir, line: inst.line, dirs: []string{e.workdir}, history: "WORKDIR " + e.workdir})
	return nil
}

func (e *stageEval) doEXPOSE(inst instruction) error {
	if err := e.noFlags(inst); err != nil {
		return err
	}
	w, err := e.words(inst)
	if err != nil {
		return err
	}
	if e.cfg.ExposedPorts == nil {
		e.cfg.ExposedPorts = map[string]struct{}{}
	}
	for _, p := range w {
		ports, err := expandPort(p)
		if err != nil {
			return e.unsupported(inst, err.Error())
		}
		for _, k := range ports {
			e.cfg.ExposedPorts[k] = struct{}{}
		}
	}
	e.meta(inst)
	return nil
}

func (e *stageEval) doEntry(inst instruction) error {
	argv, err := execOrShell(inst, e.cfg.Shell)
	if err != nil {
		return e.unsupported(inst, err.Error())
	}
	if inst.cmd == "CMD" {
		e.cfg.Cmd = argv
		e.cmdSet = true
	} else {
		e.cfg.Entrypoint = argv
		// docker resets an inherited CMD when ENTRYPOINT is set in a stage,
		// unless that stage sets its own CMD.
		if !e.cmdSet {
			e.cfg.Cmd = nil
		}
	}
	e.meta(inst)
	return nil
}

func (e *stageEval) doCOPY(inst instruction) error {
	op, err := copyOp(inst, e.vars(), e.workdir)
	if err != nil {
		return e.unsupported(inst, err.Error())
	}
	if op.from != "" {
		from := e.df.stageNamed(op.from)
		if from == nil {
			return e.unsupported(inst, fmt.Sprintf("COPY --from=%s: not a stage of this Dockerfile (images and build contexts are not supported)", op.from))
		}
		if from.index >= e.st.index {
			return e.unsupported(inst, fmt.Sprintf("COPY --from=%s: not an earlier stage", op.from))
		}
	}
	e.ev.ops = append(e.ev.ops, op)
	return nil
}

func (e *stageEval) doRUN(inst instruction) error {
	p, ok := apkCACertificates(inst)
	if !ok {
		return e.unsupported(inst, "RUN executes code at build time, which needs a Docker daemon: "+inst.cmd+" "+inst.args)
	}
	e.ev.ops = append(e.ev.ops, layerOp{kind: opRequireFile, line: inst.line, path: p})
	e.ev.notes = append(e.ev.notes, fmt.Sprintf("line %d: `RUN %s` was not executed: the base image already ships the CA bundle (/%s) that Go and busybox read; the ca-certificates package's per-certificate files and update-ca-certificates are not installed", inst.line, inst.args, p))
	return nil
}

// apkCACertificates recognises `RUN apk [--no-cache] add [--no-cache]
// ca-certificates` and returns the file that package puts in place.
func apkCACertificates(inst instruction) (string, bool) {
	if len(inst.flags) > 0 {
		return "", false
	}
	w := strings.Fields(inst.args)
	if len(w) < 3 || w[0] != "apk" {
		return "", false
	}
	sawAdd := false
	for _, a := range w[1:] {
		switch {
		case a == "add" && !sawAdd:
			sawAdd = true
		case a == "--no-cache" || a == "--update" || a == "-U" || a == "--update-cache":
		case sawAdd && (a == "ca-certificates" || a == "ca-certificates-bundle"):
		default:
			return "", false
		}
	}
	if !sawAdd {
		return "", false
	}
	return "etc/ssl/certs/ca-certificates.crt", true
}

func copyOp(inst instruction, vars map[string]string, workdir string) (layerOp, error) {
	op := layerOp{kind: opCopy, line: inst.line}
	for _, f := range inst.flags {
		switch {
		case strings.HasPrefix(f, "--from="):
			op.from = strings.TrimPrefix(f, "--from=")
		default:
			return op, fmt.Errorf("COPY flag %s is not supported", f)
		}
	}
	var w []string
	if strings.HasPrefix(strings.TrimSpace(inst.args), "[") {
		if err := json.Unmarshal([]byte(inst.args), &w); err != nil {
			return op, fmt.Errorf("COPY: malformed JSON form")
		}
		for i := range w {
			w[i] = expandVars(w[i], vars)
		}
	} else {
		var err error
		if w, err = splitWords(inst.args, vars); err != nil {
			return op, err
		}
	}
	if len(w) < 2 {
		return op, fmt.Errorf("COPY needs a source and a destination")
	}
	op.srcs = w[:len(w)-1]
	dest := w[len(w)-1]
	isDir := strings.HasSuffix(dest, "/")
	op.dest = joinAbs(workdir, dest)
	if isDir && !strings.HasSuffix(op.dest, "/") {
		op.dest += "/"
	}
	if len(op.srcs) > 1 && !strings.HasSuffix(op.dest, "/") {
		return op, fmt.Errorf("COPY with several sources needs a destination ending in /")
	}
	op.history = "COPY " + strings.Join(op.srcs, " ") + " " + op.dest
	if op.from != "" {
		op.history = "COPY --from=" + op.from + " " + strings.Join(op.srcs, " ") + " " + op.dest
	}
	return op, nil
}

func joinAbs(workdir, p string) string {
	if strings.HasPrefix(p, "/") {
		return path.Clean(p) + trailingSlash(p)
	}
	return path.Clean(path.Join(workdir, p)) + trailingSlash(p)
}

func trailingSlash(p string) string {
	if strings.HasSuffix(p, "/") && p != "/" {
		return "/"
	}
	return ""
}

// execOrShell turns an ENTRYPOINT/CMD argument into argv: a JSON array is used
// as written (docker does not expand variables in it), anything else runs
// through the shell.
func execOrShell(inst instruction, shell []string) ([]string, error) {
	if err := func() error {
		if len(inst.flags) > 0 {
			return fmt.Errorf("%s flag %s", inst.cmd, inst.flags[0])
		}
		return nil
	}(); err != nil {
		return nil, err
	}
	a := strings.TrimSpace(inst.args)
	if strings.HasPrefix(a, "[") {
		var argv []string
		if err := json.Unmarshal([]byte(a), &argv); err == nil {
			return argv, nil
		}
	}
	if a == "" {
		return nil, fmt.Errorf("%s needs a command", inst.cmd)
	}
	if len(shell) == 0 {
		shell = []string{"/bin/sh", "-c"}
	}
	return append(append([]string{}, shell...), a), nil
}

func parseHealthcheck(inst instruction) (*v1.HealthConfig, error) {
	a := strings.TrimSpace(inst.args)
	if strings.EqualFold(a, "NONE") && len(inst.flags) == 0 {
		return &v1.HealthConfig{Test: []string{"NONE"}}, nil
	}
	hc := &v1.HealthConfig{}
	for _, f := range inst.flags {
		name, val, _ := strings.Cut(strings.TrimPrefix(f, "--"), "=")
		switch name {
		case "interval", "timeout", "start-period":
			d, err := time.ParseDuration(val)
			if err != nil {
				return nil, fmt.Errorf("HEALTHCHECK --%s: %v", name, err)
			}
			switch name {
			case "interval":
				hc.Interval = d
			case "timeout":
				hc.Timeout = d
			default:
				hc.StartPeriod = d
			}
		case "retries":
			n, err := strconv.Atoi(val)
			if err != nil {
				return nil, fmt.Errorf("HEALTHCHECK --retries: %v", err)
			}
			hc.Retries = n
		default:
			return nil, fmt.Errorf("HEALTHCHECK flag %s", f)
		}
	}
	rest, ok := strings.CutPrefix(a, "CMD")
	if !ok {
		return nil, fmt.Errorf("HEALTHCHECK needs CMD or NONE")
	}
	rest = strings.TrimSpace(rest)
	if strings.HasPrefix(rest, "[") {
		var argv []string
		if err := json.Unmarshal([]byte(rest), &argv); err != nil {
			return nil, fmt.Errorf("HEALTHCHECK CMD: malformed JSON form")
		}
		hc.Test = append([]string{"CMD"}, argv...)
	} else {
		hc.Test = []string{"CMD-SHELL", rest}
	}
	return hc, nil
}

func expandPort(p string) ([]string, error) {
	spec, proto, _ := strings.Cut(p, "/")
	if proto == "" {
		proto = "tcp"
	}
	lo, hi, isRange := strings.Cut(spec, "-")
	a, err := strconv.Atoi(lo)
	if err != nil || a < 0 || a > 65535 {
		return nil, fmt.Errorf("EXPOSE: invalid port %q", p)
	}
	b := a
	if isRange {
		if b, err = strconv.Atoi(hi); err != nil || b < a || b > 65535 {
			return nil, fmt.Errorf("EXPOSE: invalid port range %q", p)
		}
	}
	var out []string
	for n := a; n <= b; n++ {
		out = append(out, fmt.Sprintf("%d/%s", n, proto))
	}
	return out, nil
}

// keyValues parses the `k=v k2="v 2"` form of ENV/LABEL, and the legacy
// `ENV key value with spaces` form.
func keyValues(inst instruction, vars map[string]string) ([][2]string, error) {
	first := strings.Fields(inst.args)
	if len(first) == 0 {
		return nil, fmt.Errorf("%s needs a value", inst.cmd)
	}
	if !strings.Contains(first[0], "=") {
		key, rest, _ := strings.Cut(strings.TrimSpace(inst.args), " ")
		w, err := splitWords(rest, vars)
		if err != nil {
			return nil, err
		}
		return [][2]string{{key, strings.Join(w, " ")}}, nil
	}
	w, err := splitWords(inst.args, vars)
	if err != nil {
		return nil, err
	}
	out := make([][2]string, 0, len(w))
	for _, kv := range w {
		k, v, ok := strings.Cut(kv, "=")
		if !ok {
			return nil, fmt.Errorf("%s: %q is not key=value", inst.cmd, kv)
		}
		out = append(out, [2]string{k, v})
	}
	return out, nil
}

func envMap(env []string) map[string]string {
	m := make(map[string]string, len(env))
	for _, kv := range env {
		k, v, _ := strings.Cut(kv, "=")
		m[k] = v
	}
	return m
}

func setEnv(env []string, k, v string) []string {
	for i, kv := range env {
		if name, _, _ := strings.Cut(kv, "="); name == k {
			env[i] = k + "=" + v
			return env
		}
	}
	return append(env, k+"="+v)
}

// splitWords tokenises s the way docker's shell-word lexer does for build
// instructions: whitespace separates words, single quotes are literal, double
// quotes allow $-expansion and \-escapes of " \ $, and an unquoted backslash
// escapes the next character. Variables expand from vars.
func splitWords(s string, vars map[string]string) ([]string, error) {
	var (
		words  []string
		cur    strings.Builder
		inWord bool
		i      int
	)
	emit := func() {
		if inWord {
			words = append(words, cur.String())
			cur.Reset()
			inWord = false
		}
	}
	for i < len(s) {
		c := s[i]
		switch {
		case c == ' ' || c == '\t':
			emit()
			i++
		case c == '\'':
			end := strings.IndexByte(s[i+1:], '\'')
			if end < 0 {
				return nil, fmt.Errorf("unterminated ' in %q", s)
			}
			cur.WriteString(s[i+1 : i+1+end])
			inWord = true
			i += end + 2
		case c == '"':
			j := i + 1
			var q strings.Builder
			closed := false
			for j < len(s) {
				if s[j] == '\\' && j+1 < len(s) && strings.IndexByte("\"\\$", s[j+1]) >= 0 {
					q.WriteByte(0) // protects the escaped byte from expandVars
					q.WriteByte(s[j+1])
					j += 2
					continue
				}
				if s[j] == '"' {
					closed = true
					break
				}
				q.WriteByte(s[j])
				j++
			}
			if !closed {
				return nil, fmt.Errorf("unterminated \" in %q", s)
			}
			cur.WriteString(unescapeLiteral(expandVars(q.String(), vars)))
			inWord = true
			i = j + 1
		case c == '\\' && i+1 < len(s):
			cur.WriteByte(s[i+1])
			inWord = true
			i += 2
		case c == '$':
			j := i + 1
			if j < len(s) && s[j] == '{' {
				k := strings.IndexByte(s[j:], '}')
				if k < 0 {
					return nil, fmt.Errorf("unterminated ${ in %q", s)
				}
				j += k + 1
			} else {
				for j < len(s) && isNameByte(s[j], j == i+1) {
					j++
				}
			}
			cur.WriteString(expandVars(s[i:j], vars))
			inWord = true
			i = j
		default:
			cur.WriteByte(c)
			inWord = true
			i++
		}
	}
	emit()
	return words, nil
}

func unescapeLiteral(s string) string { return strings.ReplaceAll(s, "\x00", "") }

func isNameByte(b byte, first bool) bool {
	return b == '_' || (b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z') || (!first && b >= '0' && b <= '9')
}

// expandVars substitutes $NAME, ${NAME}, ${NAME:-w}, ${NAME-w}, ${NAME:+w} and
// ${NAME+w}. An undefined variable expands to "".
func expandVars(s string, vars map[string]string) string {
	if !strings.Contains(s, "$") {
		return s
	}
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] != '$' {
			b.WriteByte(s[i])
			continue
		}
		if i+1 >= len(s) {
			b.WriteByte('$')
			break
		}
		if s[i+1] == '{' {
			end := strings.IndexByte(s[i:], '}')
			if end < 0 {
				b.WriteString(s[i:])
				break
			}
			b.WriteString(expandBraced(s[i+2:i+end], vars))
			i += end
			continue
		}
		j := i + 1
		for j < len(s) && isNameByte(s[j], j == i+1) {
			j++
		}
		if j == i+1 {
			b.WriteByte('$')
			continue
		}
		b.WriteString(vars[s[i+1:j]])
		i = j - 1
	}
	return b.String()
}

func expandBraced(expr string, vars map[string]string) string {
	for _, op := range []string{":-", ":+", "-", "+"} {
		name, word, ok := strings.Cut(expr, op)
		if !ok || name == "" {
			continue
		}
		val, set := vars[name]
		colon := strings.HasPrefix(op, ":")
		empty := !set || (colon && val == "")
		switch op[len(op)-1] {
		case '-':
			if empty {
				return expandVars(word, vars)
			}
			return val
		default: // '+'
			if empty {
				return ""
			}
			return expandVars(word, vars)
		}
	}
	return vars[expr]
}
