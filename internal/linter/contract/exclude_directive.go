package contract

// The exclusion gate — what `//forge:exclude-contract` costs.
//
// # Why this exists
//
// `//forge:exclude-contract` opts a package out of every contract rule, the
// injector and mock generation at once. That made it the cheapest way to turn
// `forge lint` green: one comment line, no reason, no review. Agents learned
// that, and stamped it on packages that were exactly what the contract system
// is for — an Authority with three interchangeable stores, an HTTP client to
// another service, a k8s Secret provisioner. Each one lost its mock, its
// decorator and its seam, and nothing said so.
//
// So the marker now costs two things.
//
//  1. A REASON. `//forge:exclude-contract: <why>`. A bare marker still excludes
//     (codegen never breaks over an opt-out's spelling) but lint refuses it —
//     the same bargain `forge:lint-disable` strikes for error-severity rules
//     (internal/linter/suppress): a reason turns a shrug into a decision a
//     reviewer can evaluate.
//
//  2. NOT BEING WRONG. The marker is refused outright on a package that does
//     OUTBOUND I/O or that owns a multi-implementation interface — the two
//     shapes the contracts and adapter skills name as needing a contract no
//     matter what. A reason cannot talk its way past those; the fix is a
//     contract.go (the adapter shape for I/O).
//
// # Why a module pass, not a per-package analyzer
//
// "Two implementations" is a fact about the MODULE: the second implementation
// of tokenauthority.Authority lives in accesstokenclient. A go/analysis pass
// sees one package; the in-process lint driver already holds every loaded
// package with full type information, so the check runs there once, over all
// of them.
//
// # The signals, and why they are low-noise
//
// I/O is a DIRECT CALL (resolved by the type checker, not by name) whose
// callee belongs to an outbound-I/O entry point: a method on *net/http.Client,
// the http.Get family, database/sql's handles, pgx, a controller-runtime or
// client-go client, a gRPC/Connect client, NATS, an OCI registry client, a
// cloud SDK client. Importing a package is not enough — a types-only package
// that names `client.Object` in a signature does no I/O. Local file I/O
// (`os`) is deliberately NOT a signal: a file-format library over a
// caller-supplied path is a legitimate pure-ish library.
//
// Multi-implementation is an EXPORTED interface declared in the package with
// at least two distinct non-test, non-generated named types in the module
// implementing it, at least one of them IN the package itself. The
// in-package requirement is what keeps the house rule — declare interfaces at
// the consumer — from tripping it: a consumer-side seam (reconcile.Observer)
// is implemented elsewhere, not by the package that declares it. A strategy
// registry (an exported `Register*` func taking the interface) is the one
// multi-impl shape the contracts skill names as legitimately excluded, and is
// exempt.
//
// # Exemptions
//
// Test-support packages (their non-test source imports "testing") exist to
// stand up fakes; refusing them for doing so would be noise. Packages whose
// component kind forge already exempts structurally from the contract rules
// (the app composition seam, supervised workers, reconciler/operator
// packages) are shaped by forge's own runtime interfaces, not by a Service.
//
// A genuinely-justified exception uses the ordinary suppression mechanism on
// the marker's line — `// forge:lint-disable-next-line <rule>: <why>` — so it
// is visible, reasoned, and reported by `--show-suppressed` like every other.

import (
	"fmt"
	"go/ast"
	"go/token"
	"go/types"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"golang.org/x/tools/go/packages"
	"golang.org/x/tools/go/types/typeutil"

	"github.com/reliant-labs/forge/internal/codegen"
	"github.com/reliant-labs/forge/internal/linter/finding"
	"github.com/reliant-labs/forge/internal/linter/suppress"
)

// Rule IDs of the exclusion gate. Stable: they appear in findings and are the
// names a `forge:lint-disable` directive suppresses.
const (
	// RuleExcludeReasonRequired refuses a bare `//forge:exclude-contract`.
	RuleExcludeReasonRequired = "forge-exclude-contract-reason"
	// RuleExcludeOutboundIO refuses the marker on a package doing outbound I/O.
	RuleExcludeOutboundIO = "forge-exclude-contract-outbound-io"
	// RuleExcludeMultiImpl refuses the marker on a package owning a
	// multi-implementation interface.
	RuleExcludeMultiImpl = "forge-exclude-contract-multi-impl"
)

// outboundIOSignal is one family of outbound-I/O entry points. A call whose
// callee's package path matches pkgPath (exactly, or as a prefix when
// prefix is set) and — when recv is non-empty — whose receiver type name is
// one of recv, marks the calling package as doing outbound I/O.
type outboundIOSignal struct {
	label   string
	pkgPath string
	prefix  bool
	// recv restricts to methods on these named receiver types. Empty means
	// any function or method in the package.
	recv []string
	// funcs restricts package-level functions to these names. Only consulted
	// when recv is empty.
	funcs []string
}

// outboundIOSignals is the catalogue. Each entry is an entry point that
// cannot be reached without leaving the process — never a type, an
// interface, or a helper that merely lives in the same package.
func outboundIOSignals() []outboundIOSignal {
	return []outboundIOSignal{
		{label: "an HTTP client call (net/http)", pkgPath: "net/http", recv: []string{"Client", "Transport"}},
		{label: "an HTTP client call (net/http)", pkgPath: "net/http", funcs: []string{"Get", "Head", "Post", "PostForm"}},
		{label: "a network dial (net)", pkgPath: "net", funcs: []string{"Dial", "DialTimeout", "DialTCP", "DialUDP", "DialUnix"}},
		{label: "a network dial (net)", pkgPath: "net", recv: []string{"Dialer"}},
		{label: "a database call (database/sql)", pkgPath: "database/sql", recv: []string{"DB", "Tx", "Conn", "Stmt"}},
		{label: "a database call (pgx)", pkgPath: "github.com/jackc/pgx", prefix: true},
		{label: "a Kubernetes API call (controller-runtime client)", pkgPath: "sigs.k8s.io/controller-runtime/pkg/client", recv: []string{"Client", "Reader", "Writer", "StatusWriter", "SubResourceClient", "SubResourceWriter"}},
		{label: "a Kubernetes API call (client-go)", pkgPath: "k8s.io/client-go/kubernetes", prefix: true},
		{label: "a Kubernetes API call (client-go)", pkgPath: "k8s.io/client-go/dynamic", prefix: true},
		{label: "a gRPC client call", pkgPath: "google.golang.org/grpc", recv: []string{"ClientConn"}},
		{label: "a gRPC client dial", pkgPath: "google.golang.org/grpc", funcs: []string{"Dial", "DialContext", "NewClient"}},
		{label: "a Connect client call", pkgPath: "connectrpc.com/connect", recv: []string{"Client"}},
		{label: "a NATS call", pkgPath: "github.com/nats-io/nats.go", prefix: true, recv: []string{"Conn", "EncodedConn", "JetStreamContext", "Subscription", "KeyValue", "ObjectStore"}},
		{label: "a NATS dial", pkgPath: "github.com/nats-io/nats.go", funcs: []string{"Connect"}},
		{label: "a NATS JetStream call", pkgPath: "github.com/nats-io/nats.go/jetstream", prefix: true},
		{label: "a Redis call", pkgPath: "github.com/redis/go-redis", prefix: true},
		{label: "an OCI registry call (go-containerregistry remote)", pkgPath: "github.com/google/go-containerregistry/pkg/v1/remote", prefix: true},
		{label: "an OCI registry call (oras)", pkgPath: "oras.land/oras-go", prefix: true},
		{label: "a cloud SDK call (AWS)", pkgPath: "github.com/aws/aws-sdk-go-v2/service", prefix: true},
		{label: "a cloud SDK call (GCP)", pkgPath: "cloud.google.com/go", prefix: true},
		{label: "a cloud SDK call (Azure)", pkgPath: "github.com/Azure/azure-sdk-for-go", prefix: true},
		{label: "a Stripe API call", pkgPath: "github.com/stripe/stripe-go", prefix: true},
		{label: "an S3-compatible call (minio)", pkgPath: "github.com/minio/minio-go", prefix: true},
	}
}

// ioSignalFor reports the label of the outbound-I/O family fn belongs to.
func ioSignalFor(fn *types.Func) (string, bool) {
	if fn == nil || fn.Pkg() == nil {
		return "", false
	}
	path := fn.Pkg().Path()
	recvName := ""
	if sig, ok := fn.Type().(*types.Signature); ok && sig.Recv() != nil {
		recvName = namedTypeName(sig.Recv().Type())
	}
	for _, s := range outboundIOSignals() {
		if s.prefix {
			if path != s.pkgPath && !strings.HasPrefix(path, s.pkgPath+"/") {
				continue
			}
		} else if path != s.pkgPath {
			continue
		}
		switch {
		case len(s.recv) > 0:
			if recvName != "" && contains(s.recv, recvName) {
				return s.label, true
			}
		case len(s.funcs) > 0:
			if recvName == "" && contains(s.funcs, fn.Name()) {
				return s.label, true
			}
		default:
			return s.label, true
		}
	}
	return "", false
}

// namedTypeName returns the name of t's underlying named type (through one
// pointer), or "" for an unnamed type. An interface method's receiver is the
// interface itself, so `client.Client.Get` resolves to "Client".
func namedTypeName(t types.Type) string {
	if p, ok := t.(*types.Pointer); ok {
		t = p.Elem()
	}
	switch n := t.(type) {
	case *types.Named:
		return n.Obj().Name()
	case *types.Alias:
		return namedTypeName(types.Unalias(n))
	}
	return ""
}

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

// ExcludeFinding is one refusal of an exclude-contract marker.
type ExcludeFinding struct {
	Rule     string
	Severity finding.Severity
	File     string // absolute
	Line     int
	Message  string
	FixHint  string
}

// CheckExcludeDirectives applies the exclusion gate to every root package in
// pkgs (the packages `forge lint` was asked about, loaded with types). The
// multi-implementation search spans every loaded package, so callers should
// load the whole module (`./...`) for an accurate answer; a narrower load can
// only miss an implementation, never invent one.
func CheckExcludeDirectives(pkgs []*packages.Package) []ExcludeFinding {
	// De-duplicate test variants: `Tests: true` loads `p`, `p [p.test]` and
	// `p_test`; the gate is about the package's own non-test source, which
	// the plain variant carries.
	roots := map[string]*packages.Package{}
	for _, p := range pkgs {
		if p.Types == nil || strings.HasSuffix(p.PkgPath, "_test") || strings.HasSuffix(p.PkgPath, ".test") {
			continue
		}
		if p.ID != p.PkgPath {
			// A test variant (`p [p.test]`); prefer the plain package.
			if _, ok := roots[p.PkgPath]; ok {
				continue
			}
		}
		roots[p.PkgPath] = p
	}

	// Every named type in the MAIN module's non-test, non-generated source,
	// for the implementations search. Test variants (`p [p.test]`) re-declare
	// the same types under a second types.Package; keying on path+name counts
	// each declaration once.
	var candidates []*types.TypeName
	seen := map[string]bool{}
	packages.Visit(pkgs, nil, func(p *packages.Package) {
		if p.Types == nil || p.TypesInfo == nil || p.Module == nil || !p.Module.Main || p.ID != p.PkgPath {
			return
		}
		for ident, obj := range p.TypesInfo.Defs {
			tn, ok := obj.(*types.TypeName)
			if !ok || tn.IsAlias() || tn.Parent() != tn.Pkg().Scope() {
				continue
			}
			key := tn.Pkg().Path() + "." + tn.Name()
			if seen[key] {
				continue
			}
			file := p.Fset.Position(ident.Pos()).Filename
			if isNonSourceFile(file) {
				continue
			}
			if _, isIface := tn.Type().Underlying().(*types.Interface); isIface {
				continue
			}
			seen[key] = true
			candidates = append(candidates, tn)
		}
	})

	paths := make([]string, 0, len(roots))
	for path := range roots {
		paths = append(paths, path)
	}
	sort.Strings(paths)

	var out []ExcludeFinding
	for _, path := range paths {
		p := roots[path]
		dir := pkgDir(p)
		if dir == "" {
			continue
		}
		marker, ok := codegen.FindExcludeContractMarker(dir)
		if !ok {
			continue
		}
		out = append(out, checkOnePackage(p, marker, candidates)...)
	}
	return applyDirectiveSuppressions(out)
}

func checkOnePackage(p *packages.Package, marker codegen.ExcludeContractMarker, candidates []*types.TypeName) []ExcludeFinding {
	var out []ExcludeFinding
	if marker.Reason == "" {
		out = append(out, ExcludeFinding{
			Rule:     RuleExcludeReasonRequired,
			Severity: finding.SeverityError,
			File:     marker.File,
			Line:     marker.Line,
			Message: fmt.Sprintf("package %s is excluded from the contract rules with a bare `//forge:exclude-contract` — "+
				"the marker requires a reason", p.PkgPath),
			FixHint: "write why forge should not manage this package: `//forge:exclude-contract: <why>` " +
				"(pure functions, test-only support, API/CRD types, a strategy registry). If none of those is true, " +
				"the package wants a contract.go instead — see `forge skill load contracts`",
		})
	}
	if isTestSupportPackage(p) || isStructurallyExemptKind(p) {
		return out
	}
	if label, pos, ok := firstOutboundIOCall(p); ok {
		out = append(out, ExcludeFinding{
			Rule:     RuleExcludeOutboundIO,
			Severity: finding.SeverityError,
			File:     marker.File,
			Line:     marker.Line,
			Message: fmt.Sprintf("package %s does outbound I/O (%s at %s) and so cannot opt out of the contract "+
				"system: an outbound boundary is exactly what a contract's mock and decorator are for",
				p.PkgPath, label, shortPos(pos)),
			FixHint: "remove `//forge:exclude-contract` and make this an adapter: a contract.go carrying " +
				"`// forge:outbound-io` with a narrow Service interface in domain types, and `// forge:constructor` " +
				"on `func New(Deps)` — see `forge skill load adapter`",
		})
	}
	if iface, impls, ok := multiImplInterface(p, candidates); ok {
		out = append(out, ExcludeFinding{
			Rule:     RuleExcludeMultiImpl,
			Severity: finding.SeverityError,
			File:     marker.File,
			Line:     marker.Line,
			Message: fmt.Sprintf("package %s declares interface %s with %d implementations in the module (%s) and so "+
				"cannot opt out of the contract system: an interface with interchangeable implementations IS the contract",
				p.PkgPath, iface, len(impls), strings.Join(impls, ", ")),
			FixHint: fmt.Sprintf("remove `//forge:exclude-contract` and move %s into a contract.go (mark it "+
				"`//forge:contract` if it is not named Service) — see `forge skill load contracts`. A strategy "+
				"registry is exempt when it exposes a `Register…(%s)` func", iface, iface),
		})
	}
	return out
}

// firstOutboundIOCall returns the first (by position) call in p's non-test,
// non-generated source whose callee is an outbound-I/O entry point.
func firstOutboundIOCall(p *packages.Package) (string, token.Position, bool) {
	type hit struct {
		label string
		pos   token.Position
	}
	var hits []hit
	for _, f := range p.Syntax {
		file := p.Fset.Position(f.Pos()).Filename
		if isNonSourceFile(file) {
			continue
		}
		ast.Inspect(f, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			fn, ok := typeutil.Callee(p.TypesInfo, call).(*types.Func)
			if !ok {
				return true
			}
			if label, ok := ioSignalFor(fn); ok {
				hits = append(hits, hit{label: label, pos: p.Fset.Position(call.Pos())})
			}
			return true
		})
	}
	if len(hits) == 0 {
		return "", token.Position{}, false
	}
	sort.Slice(hits, func(i, j int) bool {
		if hits[i].pos.Filename != hits[j].pos.Filename {
			return hits[i].pos.Filename < hits[j].pos.Filename
		}
		return hits[i].pos.Offset < hits[j].pos.Offset
	})
	return hits[0].label, hits[0].pos, true
}

// multiImplInterface returns the first exported, non-empty interface declared
// in p (in non-test, non-generated source) that has >= 2 implementations in
// the module, at least one of them declared in p. A strategy registry — an
// exported `Register*` func accepting the interface — is exempt.
func multiImplInterface(p *packages.Package, candidates []*types.TypeName) (string, []string, bool) {
	scope := p.Types.Scope()
	names := scope.Names() // sorted
	for _, name := range names {
		tn, ok := scope.Lookup(name).(*types.TypeName)
		if !ok || !tn.Exported() || tn.IsAlias() {
			continue
		}
		iface, ok := tn.Type().Underlying().(*types.Interface)
		if !ok || iface.NumMethods() == 0 {
			continue
		}
		if isNonSourceFile(p.Fset.Position(tn.Pos()).Filename) {
			continue
		}
		if hasRegistryFunc(p, tn.Type()) {
			continue
		}
		var impls []string
		inPkg := false
		for _, c := range candidates {
			t := c.Type()
			if !types.Implements(t, iface) && !types.Implements(types.NewPointer(t), iface) {
				continue
			}
			if c.Pkg() == tn.Pkg() || c.Pkg().Path() == tn.Pkg().Path() {
				inPkg = true
				impls = append(impls, c.Name())
			} else {
				impls = append(impls, c.Pkg().Name()+"."+c.Name())
			}
		}
		if len(impls) >= 2 && inPkg {
			sort.Strings(impls)
			return name, impls, true
		}
	}
	return "", nil, false
}

// hasRegistryFunc reports whether p exports a `Register*` function — or a
// `Register*` method on one of its types (`(*Registry).Register`) — taking t:
// the strategy-registry shape the contracts skill names as a legitimate
// exclusion.
func hasRegistryFunc(p *packages.Package, t types.Type) bool {
	takesT := func(fn *types.Func) bool {
		if !fn.Exported() || !strings.HasPrefix(fn.Name(), "Register") {
			return false
		}
		params := fn.Type().(*types.Signature).Params()
		for i := 0; i < params.Len(); i++ {
			if types.Identical(params.At(i).Type(), t) {
				return true
			}
		}
		return false
	}
	scope := p.Types.Scope()
	for _, name := range scope.Names() {
		switch obj := scope.Lookup(name).(type) {
		case *types.Func:
			if takesT(obj) {
				return true
			}
		case *types.TypeName:
			named, ok := obj.Type().(*types.Named)
			if !ok {
				continue
			}
			for i := 0; i < named.NumMethods(); i++ {
				if takesT(named.Method(i)) {
					return true
				}
			}
		}
	}
	return false
}

// isTestSupportPackage reports whether p is test-support code: its non-test
// source imports "testing" (an httptest fake, a policy-enforcing test server).
// Such a package stands up fakes by design.
func isTestSupportPackage(p *packages.Package) bool {
	for _, f := range p.Syntax {
		if isNonSourceFile(p.Fset.Position(f.Pos()).Filename) {
			continue
		}
		for _, imp := range f.Imports {
			if strings.Trim(imp.Path.Value, `"`) == "testing" {
				return true
			}
		}
	}
	return false
}

// isStructurallyExemptKind reports whether p is a component kind forge's
// contract rules already exempt by STRUCTURE — shaped by a forge runtime
// interface, not by a Service: the app composition seam, a supervised worker,
// or a controller-runtime reconciler package (it declares a
// `SetupWithManager` method, the operator lifecycle). Those packages do I/O
// by nature, and the exclusion there is not what hides a missing contract.
func isStructurallyExemptKind(p *packages.Package) bool {
	path := p.PkgPath
	if path == "internal/app" || strings.HasSuffix(path, "/internal/app") {
		return true
	}
	if strings.Contains(path, "/internal/workers/") || strings.HasPrefix(path, "internal/workers/") {
		return hasMethods(p, workerLifecycleMethods...)
	}
	return hasMethods(p, "SetupWithManager")
}

// hasMethods reports whether some type declared in p's non-test source has
// every one of names as a method.
func hasMethods(p *packages.Package, names ...string) bool {
	found := map[string]bool{}
	for _, f := range p.Syntax {
		if isNonSourceFile(p.Fset.Position(f.Pos()).Filename) {
			continue
		}
		for _, decl := range f.Decls {
			fd, ok := decl.(*ast.FuncDecl)
			if ok && fd.Recv != nil && len(fd.Recv.List) > 0 {
				found[fd.Name.Name] = true
			}
		}
	}
	for _, n := range names {
		if !found[n] {
			return false
		}
	}
	return true
}

// isNonSourceFile reports whether file is test or generated code — neither
// is the package's own hand-written surface.
func isNonSourceFile(file string) bool {
	base := filepath.Base(file)
	return strings.HasSuffix(base, "_test.go") || strings.HasSuffix(base, "_gen.go") ||
		strings.HasPrefix(base, "zz_generated")
}

// pkgDir is p's source directory.
func pkgDir(p *packages.Package) string {
	for _, f := range p.GoFiles {
		return filepath.Dir(f)
	}
	return ""
}

func shortPos(pos token.Position) string {
	return fmt.Sprintf("%s:%d", filepath.Base(pos.Filename), pos.Line)
}

// applyDirectiveSuppressions honours the ordinary line/file suppression
// directives (internal/linter/suppress) in the MARKER's file, so a genuinely
// justified exception is written the same way as every other one —
// `// forge:lint-disable-next-line <rule>: <why>` above the marker — and a
// reasonless suppression of these error-severity rules is itself reported.
func applyDirectiveSuppressions(in []ExcludeFinding) []ExcludeFinding {
	byFile := map[string][]ExcludeFinding{}
	var order []string
	for _, f := range in {
		if _, ok := byFile[f.File]; !ok {
			order = append(order, f.File)
		}
		byFile[f.File] = append(byFile[f.File], f)
	}
	var out []ExcludeFinding
	for _, file := range order {
		fs := byFile[file]
		content, err := os.ReadFile(file)
		if err != nil {
			out = append(out, fs...)
			continue
		}
		lines := strings.Split(string(content), "\n")
		for _, f := range fs {
			kept, violations := suppressAtMarker(string(content), lines, f)
			if kept {
				out = append(out, f)
			}
			for _, v := range violations {
				out = append(out, ExcludeFinding{Rule: v.Rule, Severity: v.Severity, File: file, Line: v.Line, Message: v.Message, FixHint: v.Remediation})
			}
		}
	}
	return out
}

// suppressAtMarker applies the file's suppression directives to one finding
// anchored on the marker line. A marker can draw two refusals (outbound I/O
// AND a multi-impl interface), and each wants its own reasoned allowance; a
// second `forge:lint-disable-next-line` necessarily sits between the first and
// the marker. So the contiguous block of next-line directives directly above
// the marker is read as ONE block that all targets the marker: the finding is
// checked at the marker and at each line of that block, and is suppressed if
// any of those positions is covered. Everything else — reasons, the
// missing-reason violation, file/block scopes — is suppress.Apply's own.
func suppressAtMarker(content string, lines []string, f ExcludeFinding) (kept bool, violations []finding.Finding) {
	anchors := []int{f.Line}
	for l := f.Line - 1; l >= 1 && l-1 < len(lines); l-- {
		if !strings.Contains(lines[l-1], suppress.TokenDisableNextLine) {
			break
		}
		// A directive on line l covers line l+1; checking the finding at
		// l+1 asks exactly "does the directive on l cover it".
		if l+1 != f.Line {
			anchors = append(anchors, l+1)
		}
	}
	for _, line := range anchors {
		res := suppress.Apply(content, []finding.Finding{{
			Rule: f.Rule, Severity: f.Severity, File: f.File, Line: line, Message: f.Message,
		}})
		if len(res.Kept) == 0 {
			return false, res.Violations
		}
	}
	return true, nil
}
