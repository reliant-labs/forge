// Package codegen — crd_kcl_parse.go is the controller-tools driver: it loads
// a project's api/<version> packages and projects the CRD Go types into
// apiextensions CustomResourceDefinition objects.
//
// This is deliberately the ONLY file in forge that imports controller-tools.
// The rest of the CRD pipeline (crd_kcl_gen.go) works on the resulting
// apiextensions types, which forge already depends on via controller-runtime.
// Keeping the heavy, opinionated dependency behind one function means the KCL
// lowering can be tested with hand-built CRD values and no package loading at
// all — which matters, because loading real Go packages costs seconds and
// needs a complete, compiling module on disk.
package codegen

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"sigs.k8s.io/controller-tools/pkg/crd"
	"sigs.k8s.io/controller-tools/pkg/genall"
	"sigs.k8s.io/controller-tools/pkg/markers"
)

// ControllerToolsVersion is the controller-tools release whose schema
// semantics this build projects under.
//
// It is stamped into every generated CRD's controller-gen.kubebuilder.io/version
// annotation. It must be updated in lockstep with the controller-tools version
// in forge's go.mod — a stale value here would misreport which rules produced
// a schema, which is exactly the provenance question someone asks after a bump
// changes how a Go construct projects.
const ControllerToolsVersion = "v0.20.1"

// CRDAPIDir is the conventional location of a forge project's CRD Go types.
const CRDAPIDir = "api"

// LoadCRDsFromGoTypes projects every CRD declared under <projectDir>/api/...
// into its apiextensions form, using controller-tools' own marker collection
// and schema derivation.
//
// It returns nil (no error) when the project declares no CRD types at all:
// most forge projects have no operator, and their `forge generate` must not
// fail or emit an empty module.
func LoadCRDsFromGoTypes(projectDir string) ([]CRDDoc, error) {
	apiDir := filepath.Join(projectDir, CRDAPIDir)
	if info, err := os.Stat(apiDir); err != nil || !info.IsDir() {
		return nil, nil
	}

	// Package loading resolves imports relative to the working directory, so
	// the load has to happen with the project as cwd. forge's pipeline runs
	// from the project root already, but `forge generate --dir` and the tests
	// do not, and a silently-empty CRD set is the worst possible outcome
	// here — it deletes every CRD from the manifest.
	restore, err := chdir(projectDir)
	if err != nil {
		return nil, fmt.Errorf("enter project dir: %w", err)
	}
	defer restore()

	gen := crd.Generator{
		// 0 == drop descriptions entirely. A CRD's descriptions are the Go
		// doc comments, and carrying them would multiply this manifest's size
		// several-fold for text no controller reads. The API server ignores
		// them; kubectl explain is the only consumer, and that is not worth a
		// manifest that dwarfs every other object in the render.
		MaxDescLen: ptr(0),
	}

	var asGen genall.Generator = gen
	roots, err := genall.Generators{&asGen}.ForRoots("./" + CRDAPIDir + "/...")
	if err != nil {
		return nil, fmt.Errorf("load api packages: %w", err)
	}

	reg := &markers.Registry{}
	if err := gen.RegisterMarkers(reg); err != nil {
		return nil, fmt.Errorf("register CRD markers: %w", err)
	}

	parser := &crd.Parser{
		Collector: roots.Collector,
		Checker:   roots.Checker,
	}
	crd.AddKnownTypes(parser)
	for _, root := range roots.Roots {
		parser.NeedPackage(root)
	}

	// No metav1 import means no Kubernetes API types in this tree at all —
	// a project with an api/ directory that is not a CRD package. Not an
	// error; just nothing to generate.
	metaPkg := crd.FindMetav1(roots.Roots)
	if metaPkg == nil {
		return nil, nil
	}

	kubeKinds := crd.FindKubeKinds(parser, metaPkg)
	if len(kubeKinds) == 0 {
		return nil, nil
	}

	var docs []CRDDoc
	for _, gv := range kubeKinds {
		parser.NeedCRDFor(gv, nil)
		c, ok := parser.CustomResourceDefinitions[gv]
		if !ok {
			continue
		}
		docs = append(docs, CRDDoc{
			Kind:   gv.Kind,
			Lambda: CRDLambdaName(gv.Kind),
			CRD:    c,
		})
	}

	// A project that HAS CRD types but from which we projected NONE is the
	// dangerous outcome, and it is the one worth failing on: emitting a module
	// with no lambdas removes every CRD from the render, which uninstalls them
	// from the cluster and prunes the live CRs. Report the loader's diagnostics
	// in that case, since they are what explains the empty result.
	//
	// A NON-empty result with diagnostics present is NOT failed, deliberately.
	// controller-tools type-checks each package in isolation, so it reports
	// benign "undefined: <pkg>" diagnostics for identifiers resolved through
	// imports it did not need to load — control-plane's own api/v1alpha1
	// produces two ("undefined: schema", from k8s.io/apimachinery's schema
	// package used only by the GroupVersion var). The controller-gen BINARY
	// exits 0 on exactly this input and emits complete, correct CRDs; treating
	// its diagnostics as fatal would make `forge generate` fail on a tree that
	// controller-gen considers perfectly valid.
	//
	// The real guard against a silently-missing CRD is not this error check —
	// it is that the CRD set is derived from the packages rather than
	// enumerated by hand, so a new type appears without anyone editing a list.
	if len(docs) == 0 {
		var loadErrs []string
		for _, root := range roots.Roots {
			for _, e := range root.Errors {
				loadErrs = append(loadErrs, e.Error())
			}
		}
		if len(loadErrs) > 0 {
			sort.Strings(loadErrs)
			return nil, fmt.Errorf("found %d Kubernetes kind(s) under %s but projected no CRDs; controller-tools reported:\n  %s",
				len(kubeKinds), apiDir, strings.Join(loadErrs, "\n  "))
		}
	}

	sort.Slice(docs, func(i, j int) bool { return docs[i].Kind < docs[j].Kind })
	return docs, nil
}

// CRDLambdaName maps a CR kind to its generated KCL lambda name:
// "SimpleBackend" -> "simplebackend_crd". Lowercasing the whole kind (rather
// than snake-casing it) matches the hand-authored spelling this generator
// replaces, so an existing project's call sites keep working unchanged.
func CRDLambdaName(kind string) string {
	return strings.ToLower(kind) + "_crd"
}

// toGenericJSON round-trips a typed value through its JSON encoding into the
// generic map/slice tree the KCL renderer walks. See crdToKCLValue for why the
// projection goes through JSON rather than a hand-written field walk.
func toGenericJSON(v any) (any, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return nil, fmt.Errorf("encode: %w", err)
	}
	var out any
	if err := json.Unmarshal(b, &out); err != nil {
		return nil, fmt.Errorf("decode: %w", err)
	}
	return out, nil
}

func ptr[T any](v T) *T { return &v }

// chdir changes the working directory and returns a function restoring it.
func chdir(dir string) (func(), error) {
	prev, err := os.Getwd()
	if err != nil {
		return nil, err
	}
	if err := os.Chdir(dir); err != nil {
		return nil, err
	}
	return func() { _ = os.Chdir(prev) }, nil
}
