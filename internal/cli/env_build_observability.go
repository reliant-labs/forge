package cli

// The observability files of a release bundle (console v2 contract K4).
//
// A bundle carries, beside its manifests, every dashboard the project's
// release should show and the metrics schema of the binary it ships:
//
//	observability/dashboards/<name>.json   see obsdash.CollectBundleSet
//	observability/metrics-schema.json      bin/observability/metrics-schema.json
//
// Nothing applies them; the control plane reads them when the release is
// promoted. They are validated when the release is CUT, with file:line errors,
// because a dashboard that fails in the console is found by a customer.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"

	"github.com/reliant-labs/forge/internal/bundle"
	"github.com/reliant-labs/forge/internal/obsdash"
	"github.com/reliant-labs/forge/pkg/release"
	"github.com/reliant-labs/forge/pkg/servicecatalog"
)

// collectObservabilityFiles assembles the bundle's observability set from the
// project and the build output directory ("" means bin).
func collectObservabilityFiles(projectDir, outputDir string) (obsdash.BundleSet, error) {
	return collectRenderedObservabilityFiles(projectDir, outputDir, nil)
}

func collectRenderedObservabilityFiles(projectDir, outputDir string, resolved *obsdash.Spec) (obsdash.BundleSet, error) {
	if outputDir == "" {
		outputDir = "bin"
	}
	if !filepath.IsAbs(outputDir) {
		outputDir = filepath.Join(projectDir, outputDir)
	}
	in := obsdash.BundleSetInput{ProjectDir: projectDir, Resolved: resolved}
	raw, err := os.ReadFile(filepath.Join(outputDir, filepath.FromSlash(MetricsSchemaArtifact)))
	switch {
	case err == nil:
		in.MetricsSchema = raw
	case os.IsNotExist(err):
	default:
		return obsdash.BundleSet{}, err
	}
	return obsdash.CollectBundleSet(in)
}

// dashboardDeclarationFor validates preview dashboards from precisely the
// selected declaration. It neither re-renders nor writes global dashboard state.
func dashboardDeclarationFor(ctx context.Context, projectDir, env string, probed *KCLEntities) (*KCLEntities, error) {
	entities, err := declarationFor(ctx, projectDir, env, probed)
	if err != nil {
		return nil, err
	}
	if _, err := renderedObservabilitySet(projectDir, "", entities); err != nil {
		return nil, err
	}
	return entities, nil
}

func renderedObservabilitySet(projectDir, outputDir string, entities *KCLEntities) (obsdash.BundleSet, error) {
	resolved, err := dashboardSpecForEntities(projectDir, entities)
	if err != nil {
		return obsdash.BundleSet{}, err
	}
	return collectRenderedObservabilityFiles(projectDir, outputDir, resolved)
}

// withholdServiceCatalog drops the service catalog from files when reader
// does not name [release.BundleFeatureServiceCatalog], and reports whether it
// did. A nil reader (this forge's own) accepts it.
func withholdServiceCatalog(files []bundle.ObservabilityFile, reader *release.BundleReader) ([]bundle.ObservabilityFile, bool) {
	if reader.Supports(release.BundleFeatureServiceCatalog) {
		return files, false
	}
	out := make([]bundle.ObservabilityFile, 0, len(files))
	for _, f := range files {
		if f.Path != servicecatalog.BundlePath {
			out = append(out, f)
		}
	}
	return out, len(out) != len(files)
}

// catalogWithheldNoted holds the envs whose withheld-catalog note this
// process has printed: a deploy renders the bundle more than once (plan,
// then write), and the fact is worth saying once.
var catalogWithheldNoted sync.Map

// noteServiceCatalogWithheld prints, once per env, that the catalog stayed
// behind.
func noteServiceCatalogWithheld(w io.Writer, env string) {
	if _, seen := catalogWithheldNoted.LoadOrStore(env, true); seen {
		return
	}
	fmt.Fprintf(w, "[bundle] Note: the service catalog was not uploaded: this control plane does not accept it yet, "+
		"so the RPC explorer will lack env %s's declared routes. The deploy is unaffected.\n", env)
}

// bundleObservabilityFiles is the set as bundle files. A set that does not
// validate fails a strict write (the build and cut) and is dropped with a
// warning otherwise.
func bundleObservabilityFiles(projectDir string, in bundleBuildInputs, entities *KCLEntities) ([]bundle.ObservabilityFile, error) {
	set, err := renderedObservabilitySet(projectDir, in.OutputDir, entities)
	if err != nil {
		if in.StrictObservability {
			return nil, err
		}
		fmt.Fprintf(in.errWriter(), "[bundle] Warning: the bundle carries no dashboards, because they do not validate:\n%v\n", err)
		return nil, nil
	}
	for _, n := range set.Notes {
		fmt.Fprintf(in.errWriter(), "[bundle] Note: %s\n", n)
	}
	files := obsdash.BundleFiles(set)
	out := make([]bundle.ObservabilityFile, 0, len(files))
	for _, f := range files {
		out = append(out, bundle.ObservabilityFile{Path: f.Path, Data: f.Data})
	}
	raw, readErr := os.ReadFile(filepath.Join(projectDir, filepath.FromSlash(servicecatalog.GeneratedPath)))
	if readErr == nil {
		if _, err := servicecatalog.Decode(raw); err != nil {
			return nil, err
		}
		// gen/ keeps the indented form for reviewable diffs; the bundle, which
		// the control plane serves to a browser, carries the compact one (a
		// third smaller for a large project).
		var compact bytes.Buffer
		if err := json.Compact(&compact, raw); err != nil {
			return nil, err
		}
		out = append(out, bundle.ObservabilityFile{Path: servicecatalog.BundlePath, Data: compact.Bytes()})
	} else if !os.IsNotExist(readErr) {
		return nil, readErr
	}
	return out, nil
}
