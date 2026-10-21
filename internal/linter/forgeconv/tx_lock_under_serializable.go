// File: internal/linter/forgeconv/tx_lock_under_serializable.go
//
// forgeconv-tx-lock-under-serializable: a SERIALIZABLE (or REPEATABLE READ)
// transaction whose body takes a lock — SELECT … FOR UPDATE / FOR NO KEY
// UPDATE, bun's .For("UPDATE"), pg_advisory_xact_lock / pg_advisory_lock,
// or LOCK TABLE.
//
// The two correctness arguments a transaction can make are exclusive:
//
//   - SERIALIZABLE: the database proves the transactions equivalent to a
//     serial order and aborts (40001) the one it cannot place. Correct with
//     no locks, provided the aborted one is retried.
//   - READ COMMITTED + a lock: the lock makes writers take turns, and each
//     statement after the wait sees the row the previous holder committed.
//
// Combining them is the trap. Under SERIALIZABLE (and REPEATABLE READ) the
// snapshot is taken at the transaction's first statement — before the lock
// wait — so the writer that waited wakes into a stale snapshot, and writing
// the row its predecessor changed fails with 40001. It survives only by being
// retried: every contended wait costs a full rollback and re-run, and with a
// retry budget of one (BeginDetached, MaxAttempts 1) it simply fails. That is
// how a port that moved a FOR UPDATE helper from READ COMMITTED onto the
// SERIALIZABLE default dropped updates under contention.
//
// The analysis is syntactic: the level is read from the options argument as
// written (orm.Serializable / orm.RepeatableRead and their With… variants,
// or an orm.TxOptions literal naming IsolationSerializable /
// IsolationRepeatableRead), and the lock from the SQL string literals (and
// the string constants they name) in the transaction's body — the closure
// passed to RunTx / RunTransaction, the package function named there, or,
// for BeginDetached, the rest of the function that opened it. A lock taken
// inside a helper the body calls is not seen. A read-only transaction
// (WithReadOnly) is not judged: it cannot write the row it locked.
//
// Warning by default; an error under `forge lint --strict`.

package forgeconv

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

const txLockUnderSerializableRule = "forgeconv-tx-lock-under-serializable"

const ormImportPath = "github.com/reliant-labs/forge/pkg/orm"

// sqlLockPatterns name the locks in SQL text, each with how the finding
// spells it.
var sqlLockPatterns = []struct {
	re   *regexp.Regexp
	name string
}{
	{regexp.MustCompile(`(?i)\bFOR\s+(NO\s+KEY\s+)?UPDATE\b`), "SELECT … FOR UPDATE"},
	{regexp.MustCompile(`(?i)\bpg_(try_)?advisory_(xact_)?lock\s*\(`), "pg_advisory_xact_lock"},
	{regexp.MustCompile(`(?i)\bLOCK\s+TABLE\b`), "LOCK TABLE"},
}

// bunForLock is the argument of a bun builder's .For("…") that locks rows
// for writing.
var bunForLock = regexp.MustCompile(`(?i)^\s*(NO\s+KEY\s+)?UPDATE\b`)

// LintTxLockUnderSerializable walks rootDir for Go files and reports
// SERIALIZABLE transactions that take a lock. Generated files (gen/,
// *_gen.go), vendor/, node_modules/, testdata/ and pkg/orm itself are
// skipped. Findings are in deterministic order.
func LintTxLockUnderSerializable(rootDir string, opts LintOptions) (Result, error) {
	var files []string
	err := filepath.WalkDir(rootDir, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			switch d.Name() {
			case ".git", "node_modules", "vendor", "gen", "testdata", ".forge":
				if p != rootDir {
					return filepath.SkipDir
				}
			}
			return nil
		}
		if strings.HasSuffix(p, ".go") && !strings.HasSuffix(p, "_gen.go") {
			files = append(files, p)
		}
		return nil
	})
	if err != nil {
		return Result{}, fmt.Errorf("walk %s: %w", rootDir, err)
	}
	sort.Strings(files)

	type parsed struct {
		path string
		file *ast.File
	}
	fset := token.NewFileSet()
	byDir := map[string][]parsed{}
	var dirs []string
	for _, fp := range files {
		src, rerr := os.ReadFile(fp)
		if rerr != nil || !strings.Contains(string(src), ormImportPath) {
			continue // names no orm policy
		}
		file, perr := parser.ParseFile(fset, fp, src, parser.SkipObjectResolution)
		if perr != nil {
			continue // the Go toolchain reports parse errors
		}
		dir := filepath.Dir(fp)
		if _, seen := byDir[dir]; !seen {
			dirs = append(dirs, dir)
		}
		byDir[dir] = append(byDir[dir], parsed{fp, file})
	}

	severity := SeverityWarning
	if opts.Strict {
		severity = SeverityError
	}
	var result Result
	for _, dir := range dirs {
		consts := map[string]map[string]ast.Expr{}
		funcs := map[string]map[string]*ast.FuncDecl{}
		for _, pf := range byDir[dir] {
			pkg := pf.file.Name.Name
			if consts[pkg] == nil {
				consts[pkg] = map[string]ast.Expr{}
				funcs[pkg] = map[string]*ast.FuncDecl{}
			}
			collectStringDecls(pf.file.Decls, consts[pkg])
			for _, d := range pf.file.Decls {
				if fd, ok := d.(*ast.FuncDecl); ok && fd.Recv == nil && fd.Body != nil {
					funcs[pkg][fd.Name.Name] = fd
				}
			}
		}
		for _, pf := range byDir[dir] {
			ormName := importName(pf.file, ormImportPath)
			if ormName == "" {
				continue // pkg/orm itself, or a file that names no policy
			}
			rel, relErr := filepath.Rel(rootDir, pf.path)
			if relErr != nil {
				rel = pf.path
			}
			l := txLockLint{
				fset: fset, rel: filepath.ToSlash(rel), orm: ormName, severity: severity,
				consts: consts[pf.file.Name.Name], funcs: funcs[pf.file.Name.Name],
			}
			result.Findings = append(result.Findings, l.file(pf.file)...)
		}
	}
	sort.SliceStable(result.Findings, func(i, j int) bool {
		a, b := result.Findings[i], result.Findings[j]
		if a.File != b.File {
			return a.File < b.File
		}
		return a.Line < b.Line
	})
	return result, nil
}

type txLockLint struct {
	fset     *token.FileSet
	rel      string
	orm      string
	severity Severity
	consts   map[string]ast.Expr
	funcs    map[string]*ast.FuncDecl
}

func (l txLockLint) file(file *ast.File) []Finding {
	var findings []Finding
	// The bodies enclosing the current node, innermost last: BeginDetached
	// holds its transaction for the rest of the function that opened it.
	var bodies []*ast.BlockStmt
	var visit func(n ast.Node) bool
	visit = func(n ast.Node) bool {
		switch n := n.(type) {
		case *ast.FuncDecl:
			if n.Body != nil {
				bodies = append(bodies, n.Body)
				ast.Inspect(n.Body, visit)
				bodies = bodies[:len(bodies)-1]
			}
			return false
		case *ast.FuncLit:
			bodies = append(bodies, n.Body)
			ast.Inspect(n.Body, visit)
			bodies = bodies[:len(bodies)-1]
			return false
		case *ast.CallExpr:
			if f, ok := l.call(n, bodies); ok {
				findings = append(findings, f)
			}
		}
		return true
	}
	ast.Inspect(file, visit)
	return findings
}

// call judges one call: a SERIALIZABLE opener whose body takes a lock.
func (l txLockLint) call(call *ast.CallExpr, bodies []*ast.BlockStmt) (Finding, bool) {
	sel, ok := call.Fun.(*ast.SelectorExpr)
	if !ok {
		return Finding{}, false
	}
	var policy ast.Expr
	var body []ast.Node
	switch sel.Sel.Name {
	case "RunTx", "RunTransaction":
		if len(call.Args) != 3 {
			return Finding{}, false
		}
		policy = call.Args[1]
		switch fn := call.Args[2].(type) {
		case *ast.FuncLit:
			body = []ast.Node{fn.Body}
		case *ast.Ident:
			if fd := l.funcs[fn.Name]; fd != nil {
				body = []ast.Node{fd.Body}
			}
		}
	case "BeginDetached":
		if len(call.Args) != 2 || len(bodies) == 0 {
			return Finding{}, false
		}
		policy = call.Args[1]
		// The rest of the opening function, from the call on.
		for _, st := range bodies[len(bodies)-1].List {
			if st.End() > call.Pos() {
				body = append(body, st)
			}
		}
	default:
		return Finding{}, false
	}
	level, ok := l.lockingLevel(policy)
	if !ok || len(body) == 0 {
		return Finding{}, false
	}
	lock, at, found := l.findLock(body, call)
	if !found {
		return Finding{}, false
	}
	return Finding{
		Rule:     txLockUnderSerializableRule,
		Severity: l.severity,
		File:     l.rel,
		Line:     l.fset.Position(call.Pos()).Line,
		Message: fmt.Sprintf("%s transaction (%s) takes a lock — %s at %s:%d. Under %s the writer that waits on the lock wakes into a snapshot taken before the wait and fails with 40001: it survives only by retrying, and a contended wait costs a full rollback and re-run",
			strings.ToUpper(level), oneLineExpr(l.fset, policy), lock, l.rel, l.fset.Position(at).Line, strings.ToUpper(level)),
		Remediation: fmt.Sprintf("if the lock is what makes this correct, run it at %s.ReadCommitted — the lock makes the writers take turns and each statement after the wait sees the committed row; if %s is what makes it correct, drop the lock. Keeping both on purpose: `// forge:lint-disable-next-line %s: <why>`",
			l.orm, strings.ToUpper(level), txLockUnderSerializableRule),
	}, true
}

// lockingLevel reports the level a policy expression names when it is one a
// lock fights: SERIALIZABLE or REPEATABLE READ, read-write.
func (l txLockLint) lockingLevel(e ast.Expr) (string, bool) {
	for {
		switch x := e.(type) {
		case *ast.ParenExpr:
			e = x.X
			continue
		case *ast.CallExpr:
			// orm.Serializable.WithMaxAttempts(n) / .WithReadOnly()
			sel, ok := x.Fun.(*ast.SelectorExpr)
			if !ok {
				return "", false
			}
			switch sel.Sel.Name {
			case "WithReadOnly":
				return "", false
			case "WithMaxAttempts":
				e = sel.X
				continue
			}
			return "", false
		case *ast.SelectorExpr:
			if !l.isORM(x.X) {
				return "", false
			}
			switch x.Sel.Name {
			case "Serializable":
				return "serializable", true
			case "RepeatableRead":
				return "repeatable read", true
			}
			return "", false
		case *ast.CompositeLit:
			sel, ok := x.Type.(*ast.SelectorExpr)
			if !ok || sel.Sel.Name != "TxOptions" || !l.isORM(sel.X) {
				return "", false
			}
			level := ""
			for _, elt := range x.Elts {
				kv, ok := elt.(*ast.KeyValueExpr)
				if !ok {
					continue
				}
				key, _ := kv.Key.(*ast.Ident)
				if key == nil {
					continue
				}
				switch key.Name {
				case "ReadOnly":
					if id, ok := kv.Value.(*ast.Ident); !ok || id.Name != "false" {
						return "", false
					}
				case "Isolation":
					v, ok := kv.Value.(*ast.SelectorExpr)
					if !ok || !l.isORM(v.X) {
						return "", false
					}
					switch v.Sel.Name {
					case "IsolationSerializable":
						level = "serializable"
					case "IsolationRepeatableRead":
						level = "repeatable read"
					}
				}
			}
			return level, level != ""
		}
		return "", false
	}
}

func (l txLockLint) isORM(x ast.Expr) bool {
	id, ok := x.(*ast.Ident)
	return ok && id.Name == l.orm
}

// findLock returns the first lock the nodes take: a locking string literal
// (or a string constant named there), or bun's .For("UPDATE"). Nested
// transaction openers are their own finding and are not looked into.
func (l txLockLint) findLock(nodes []ast.Node, opener *ast.CallExpr) (string, token.Pos, bool) {
	var name string
	var at token.Pos
	check := func(s string, pos token.Pos) bool {
		for _, p := range sqlLockPatterns {
			if p.re.MatchString(s) {
				name, at = p.name, pos
				return true
			}
		}
		return false
	}
	for _, root := range nodes {
		ast.Inspect(root, func(n ast.Node) bool {
			if at.IsValid() {
				return false
			}
			switch x := n.(type) {
			case *ast.CallExpr:
				if x != opener {
					if sel, ok := x.Fun.(*ast.SelectorExpr); ok {
						switch sel.Sel.Name {
						case "RunTx", "RunTransaction", "BeginDetached", "RunTxReadOnly":
							return false
						case "For":
							if len(x.Args) == 1 {
								if s, ok := stringLit(x.Args[0]); ok && bunForLock.MatchString(s) {
									name, at = `.For("UPDATE")`, x.Pos()
									return false
								}
							}
						}
					}
				}
			case *ast.BasicLit:
				if s, ok := stringLit(x); ok && check(s, x.Pos()) {
					return false
				}
			case *ast.Ident:
				if e, ok := l.consts[x.Name]; ok {
					if s, ok := stringLit(e); ok && check(s, x.Pos()) {
						return false
					}
				}
			}
			return true
		})
		if at.IsValid() {
			return name, at, true
		}
	}
	return "", token.NoPos, false
}

// stringLit is the value of a string literal, or of a concatenation of them.
func stringLit(e ast.Expr) (string, bool) {
	switch x := e.(type) {
	case *ast.BasicLit:
		if x.Kind != token.STRING {
			return "", false
		}
		s, err := strconv.Unquote(x.Value)
		return s, err == nil
	case *ast.BinaryExpr:
		if x.Op != token.ADD {
			return "", false
		}
		a, ok1 := stringLit(x.X)
		b, ok2 := stringLit(x.Y)
		return a + b, ok1 && ok2
	case *ast.ParenExpr:
		return stringLit(x.X)
	}
	return "", false
}

func oneLineExpr(fset *token.FileSet, e ast.Expr) string {
	var b strings.Builder
	start, end := fset.Position(e.Pos()), fset.Position(e.End())
	if start.Filename == "" {
		return "?"
	}
	src, err := os.ReadFile(start.Filename)
	if err != nil || end.Offset > len(src) {
		return "?"
	}
	b.WriteString(strings.Join(strings.Fields(string(src[start.Offset:end.Offset])), " "))
	return b.String()
}
