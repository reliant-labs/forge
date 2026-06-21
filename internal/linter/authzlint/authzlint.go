// Package authzlint is the generate-time completeness gate for forge's
// descriptor-driven authorization.
//
// The runtime library (forge/pkg/authz) fails CLOSED: a method with no
// resolvable role policy is denied. That is the last line of defense, but a
// fail-closed deny only shows up when someone calls the method. This lint is
// the FIRST line: it runs at `forge generate` and FAILS THE BUILD if any RPC
// method is not explicitly authorized, naming the method. Together they make
// "a method silently defaults to open" impossible — the lint stops the build
// before the binary exists, and if a method ever does slip through, the
// runtime denies it.
//
// A method is "explicitly authorized" when it carries one of:
//
//   - (forge.v1.method).required_roles = [...]  — an any-of role allow-list, OR
//   - (forge.v1.method).authz_public = true     — explicit any-authenticated, OR
//   - it inherits a non-empty (forge.v1.service).default_roles from its service.
//
// Anything else is a finding. Setting BOTH required_roles and authz_public on
// one method is a contradiction and is also a finding.
//
// The lint operates on proto DESCRIPTORS (protoreflect), the same source the
// runtime policy builder reads, so the two can never disagree about what
// counts as annotated. It does not touch the generate/CLI plumbing; a caller
// in the generate pipeline passes the already-parsed FileDescriptors.
package authzlint

import (
	"fmt"
	"sort"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protodesc"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/descriptorpb"

	"github.com/reliant-labs/forge/internal/linter/finding"
	forgev1 "github.com/reliant-labs/forge/pkg/forgepb"
)

// Rule names emitted by this lint, stable for `forge lint --json` consumers.
const (
	// RuleMissingPolicy fires when a method is neither annotated with an authz
	// decision nor covered by a service default_roles.
	RuleMissingPolicy = "authz-missing-policy"
	// RuleContradiction fires when a method sets both required_roles and
	// authz_public — an ambiguous decision the build must reject.
	RuleContradiction = "authz-contradictory-policy"
)

// Result is the lint outcome. It embeds the shared finding.Result so it shares
// the canonical finding vocabulary with every other internal linter, and adds
// HasErrors via that embedding.
type Result struct {
	finding.Result
}

// Lint checks every method of every service across the given file descriptors
// for an explicit authorization decision and returns the findings. An empty
// Findings slice means every method is explicitly authorized.
//
// files is the descriptor source — pass the FileDescriptors `forge generate`
// already compiled (production), or a scoped *protoregistry.Files (tests). It
// is iterated via the standard RangeFiles signature so either fits.
func Lint(files FileSource) Result {
	var res Result
	files.RangeFiles(func(fd protoreflect.FileDescriptor) bool {
		svcs := fd.Services()
		for i := 0; i < svcs.Len(); i++ {
			svc := svcs.Get(i)
			hasServiceDefault := serviceHasDefaultRoles(svc)
			ms := svc.Methods()
			for j := 0; j < ms.Len(); j++ {
				m := ms.Get(j)
				if f := checkMethod(fd, svc, m, hasServiceDefault); f != nil {
					res.Findings = append(res.Findings, *f)
				}
			}
		}
		return true
	})
	// Stable order so build output / golden tests don't flap.
	sort.Slice(res.Findings, func(i, j int) bool {
		return res.Findings[i].Message < res.Findings[j].Message
	})
	return res
}

// checkMethod returns a finding for one method, or nil if it is explicitly
// authorized.
func checkMethod(fd protoreflect.FileDescriptor, svc protoreflect.ServiceDescriptor, m protoreflect.MethodDescriptor, hasServiceDefault bool) *finding.Finding {
	procedure := "/" + string(svc.FullName()) + "/" + string(m.Name())
	public, roleCount := methodAuthz(m)

	if public && roleCount > 0 {
		return &finding.Finding{
			Rule:     RuleContradiction,
			Severity: finding.SeverityError,
			File:     fd.Path(),
			Message: fmt.Sprintf(
				"method %s sets both (forge.v1.method).authz_public and .required_roles — pick exactly one",
				procedure),
			Remediation: "Remove authz_public to keep the role restriction, or remove required_roles to make the method public.",
		}
	}
	if public || roleCount > 0 || hasServiceDefault {
		return nil // explicitly authorized
	}
	return &finding.Finding{
		Rule:     RuleMissingPolicy,
		Severity: finding.SeverityError,
		File:     fd.Path(),
		Message: fmt.Sprintf(
			"method %s has no authorization policy — declare (forge.v1.method).required_roles, set authz_public = true, or give the service a (forge.v1.service).default_roles",
			procedure),
		Remediation: "Add `option (forge.v1.method).required_roles = [\"<role>\"];` (or `authz_public = true` for a public method) to the rpc, or set service-wide default_roles.",
	}
}

// methodAuthz reads the per-method authz annotation and reports whether the
// method is marked public and how many required roles it declares.
func methodAuthz(m protoreflect.MethodDescriptor) (public bool, roleCount int) {
	opts := m.Options()
	if opts == nil {
		return false, 0
	}
	ext := proto.GetExtension(opts, forgev1.E_Method)
	mo, ok := ext.(*forgev1.MethodOptions)
	if !ok || mo == nil {
		return false, 0
	}
	return mo.GetAuthzPublic(), len(mo.GetRequiredRoles())
}

// serviceHasDefaultRoles reports whether the service declares a non-empty
// (forge.v1.service).default_roles, which every method inherits.
func serviceHasDefaultRoles(svc protoreflect.ServiceDescriptor) bool {
	opts := svc.Options()
	if opts == nil {
		return false
	}
	ext := proto.GetExtension(opts, forgev1.E_Service)
	so, ok := ext.(*forgev1.ServiceOptions)
	if !ok || so == nil {
		return false
	}
	return len(so.GetDefaultRoles()) > 0
}

// FileSource is the descriptor iterator the lint consumes. Both
// *protoregistry.Files and protoregistry.GlobalFiles satisfy it, as does any
// caller that can range its compiled FileDescriptors.
type FileSource interface {
	RangeFiles(func(protoreflect.FileDescriptor) bool)
}

// LintFiles compiles a FileDescriptorSet (e.g. the image `buf build -o` emits,
// or the descriptors the generate pipeline already has) into a registry and
// lints it. This is the convenience entry the generate/lint pipeline should
// call once it holds the project's compiled descriptors.
//
// TODO(forge-wire): wire this into the generate / `forge lint` pipeline. The
// descriptor read and lint orchestration both live in internal/cli today
// (forge_descriptor.go builds the project's descriptors; lint.go / lint_json.go
// aggregate the other linters), which is outside this change's scope. To wire:
//
//  1. In the generate-validate (or `forge lint`) step, obtain the project's
//     *descriptorpb.FileDescriptorSet (buf already builds it) and call
//     authzlint.LintFiles(fds). Alternatively pass the live *protoregistry.Files
//     to authzlint.Lint directly.
//  2. Append res.Findings to the aggregated lint findings (they already use the
//     shared internal/linter/finding vocabulary) and let res.HasErrors() gate
//     the build — this is the guardrail that makes a silently-open method
//     impossible at generate time (the runtime fail-closed deny is the backstop).
func LintFiles(fds *descriptorpb.FileDescriptorSet) (Result, error) {
	files, err := protodesc.NewFiles(fds)
	if err != nil {
		return Result{}, fmt.Errorf("authzlint: build descriptors: %w", err)
	}
	return Lint(files), nil
}
