package lint

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/descriptorpb"

	"github.com/reliant-labs/forge/internal/cliutil"
	"github.com/reliant-labs/forge/internal/linter/authzlint"
	"github.com/reliant-labs/forge/internal/linter/finding"
)

// authz-completeness lint — the generate-time first line of defense for
// forge's descriptor-driven authorization (the runtime fail-closed deny in
// forge/pkg/authz is the backstop). It fails the build when any RPC method
// lacks an explicit authorization decision (required_roles / authz_public /
// a service default_roles), naming the method.
//
// The lint operates on proto DESCRIPTORS — the same source the runtime
// policy builder reads — so the two can never disagree about what counts as
// annotated. internal/linter/authzlint owns the descriptor walk + finding
// vocabulary; this file is the CLI wiring the package's TODO(forge-wire)
// asked for: it builds the project's *descriptorpb.FileDescriptorSet via
// buf and hands it to authzlint.LintFiles, then plugs the result into the
// `forge lint` text + JSON drivers as one ordered linterStep.

// buildProjectDescriptorSet compiles the project's .proto files into a
// FileDescriptorSet using buf (`buf build --as-file-descriptor-set`).
// Imports are INCLUDED so the set is self-contained and protodesc.NewFiles
// can resolve every reference (notably google/protobuf/descriptor.proto,
// which the forge option extensions depend on) — without them the
// descriptor build fails. Including imports does not loosen the lint: the
// imported forge / google protos declare no service methods, so only the
// project's own RPCs are ever judged. Returns (nil, nil) when there is
// nothing to compile (no buf.yaml / no proto), so the caller treats it as a
// clean skip rather than an error.
func buildProjectDescriptorSet(ctx context.Context, projectDir string) (*descriptorpb.FileDescriptorSet, error) {
	if !authzCompletenessApplies(projectDir) {
		return nil, nil
	}

	tmp, err := os.CreateTemp("", "forge-authzlint-*.binpb")
	if err != nil {
		return nil, fmt.Errorf("create temp descriptor file: %w", err)
	}
	tmpPath := tmp.Name()
	_ = tmp.Close()
	defer func() { _ = os.Remove(tmpPath) }()

	cmd := exec.CommandContext(ctx, "buf", "build",
		"--as-file-descriptor-set", "-o", tmpPath)
	cmd.Dir = projectDir
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("buf build (descriptor set for authz lint) failed: %w", err)
	}

	data, err := os.ReadFile(tmpPath)
	if err != nil {
		return nil, fmt.Errorf("read built descriptor set: %w", err)
	}
	var fds descriptorpb.FileDescriptorSet
	if err := proto.Unmarshal(data, &fds); err != nil {
		return nil, fmt.Errorf("unmarshal descriptor set: %w", err)
	}
	return &fds, nil
}

// collectAuthzCompletenessFindings is the shared engine behind the text and
// JSON drivers: it builds the project descriptors and runs the authz
// completeness lint. A nil descriptor set (no buf/proto) returns no findings
// (clean skip).
func collectAuthzCompletenessFindings(ctx context.Context, projectDir string) (finding.Result, error) {
	fds, err := buildProjectDescriptorSet(ctx, projectDir)
	if err != nil {
		return finding.Result{}, err
	}
	if fds == nil {
		return finding.Result{}, nil
	}
	res, err := authzlint.LintFiles(fds)
	if err != nil {
		return finding.Result{}, err
	}
	return res.Result, nil
}

// runAuthzCompletenessLint is the text-mode driver. Errors (every authz
// finding is error-severity) gate the build.
func runAuthzCompletenessLint(ctx context.Context) error {
	fmt.Println("Running authz completeness lint...")

	res, err := collectAuthzCompletenessFindings(ctx, ".")
	if err != nil {
		return fmt.Errorf("authz completeness lint failed: %w", err)
	}
	if len(res.Findings) == 0 {
		fmt.Println("✓ every RPC has an explicit authorization decision")
		return nil
	}

	for _, f := range res.Findings {
		fmt.Printf("  ❌ [%s] %s\n", f.Rule, f.Message)
		if f.Remediation != "" {
			fmt.Printf("       %s\n", f.Remediation)
		}
	}
	if res.HasErrors() {
		return cliutil.UserErr("forge lint",
			"authorization completeness violations found",
			"",
			"give every RPC an explicit authz decision: add `(forge.v1.method).required_roles`, set `authz_public = true`, or give the service a `(forge.v1.service).default_roles` — the runtime denies un-annotated methods, so the build must stop here first")
	}
	return nil
}

// CheckAuthzCompleteness is the generate-pipeline entry point: it runs the
// authz completeness lint and returns a build-gating error (naming every
// offending RPC) when any method lacks an explicit authz decision, or nil
// when the project is clean / has no proto to check. It is the same check
// `forge lint` runs, exposed for the generate pipeline so an un-annotated
// RPC fails `forge generate` too (the runtime fail-closed deny is the
// backstop; this is the first line). When buf is unavailable the check is a
// no-op — generate must not hard-require buf on PATH for non-proto steps.
func CheckAuthzCompleteness(ctx context.Context, projectDir string) error {
	if !authzCompletenessApplies(projectDir) {
		return nil
	}
	if _, err := exec.LookPath("buf"); err != nil {
		return nil
	}
	res, err := collectAuthzCompletenessFindings(ctx, projectDir)
	if err != nil {
		return err
	}
	if !res.HasErrors() {
		return nil
	}
	var b strings.Builder
	b.WriteString("authorization completeness violations — every RPC must declare an explicit authz decision:\n")
	for _, f := range res.Findings {
		b.WriteString("  - " + f.Message + "\n")
	}
	b.WriteString("Add `(forge.v1.method).required_roles`, set `authz_public = true`, or give the service a `(forge.v1.service).default_roles`.")
	return cliutil.UserErr("forge generate (authz completeness)", b.String(), "",
		"annotate the RPCs named above; the runtime denies un-annotated methods, so generate stops here first")
}

// collectAuthzCompletenessJSON is the JSON-mode collector. The bool is the
// per-step gating verdict (true when any error finding exists).
func collectAuthzCompletenessJSON(ctx context.Context) ([]lintJSONFinding, bool, error) {
	res, err := collectAuthzCompletenessFindings(ctx, ".")
	if err != nil {
		return nil, false, err
	}
	return findingsToJSON(res.Findings), res.HasErrors(), nil
}

// authzCompletenessApplies reports whether the authz lint has anything to
// run against (buf.yaml + proto/ present under projectDir). Used by the
// pipeline step's shouldRun gate; mirrors the cheap presence checks the
// other steps use.
func authzCompletenessApplies(projectDir string) bool {
	return fileExists(filepath.Join(projectDir, "buf.yaml")) && dirExists(filepath.Join(projectDir, "proto"))
}
