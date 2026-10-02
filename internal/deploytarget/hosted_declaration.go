package deploytarget

// The DECLARATION on the wire: how a rendered shape and its provenance are
// carried to a control plane.
//
// forge does not import control-plane, so — as everywhere else in this file's
// neighbourhood — the request shapes are declared here, in proto3 JSON. Two
// encodings live here and nowhere else, because two commands send them
// (`forge env build` and `forge env deploy`, through the ensure) and a second
// copy would be a second opinion about what a field is called.

import (
	"encoding/json"
	"fmt"

	"github.com/reliant-labs/forge/pkg/release"
)

// shapeWireFields is DeployEnvironmentSpec.shape: a google.protobuf.Struct,
// whose proto3-JSON form is the object itself.
//
// It round-trips through [release.Shape.Encode] rather than marshalling the
// struct directly. Encode is the CANONICAL encoding — sorted lists, empty
// lists as `[]` and not null, bounded size — and the server decodes what
// arrives strictly into release.Shape and re-encodes it the same way. Going
// through Encode here means forge sends exactly the bytes it would print, so
// a shape a user read with `forge env shape` and the shape the control plane
// stored cannot differ.
func shapeWireFields(shape release.Shape) (map[string]any, error) {
	canonical, err := shape.Encode()
	if err != nil {
		return nil, fmt.Errorf("encode the declared shape: %w", err)
	}
	var fields map[string]any
	if err := json.Unmarshal(canonical, &fields); err != nil {
		return nil, fmt.Errorf("encode the declared shape: %w", err)
	}
	return fields, nil
}

// ProvenanceWireFields is controlplane.v1.DeploySourceProvenance: where a
// render or a release came from.
//
// THE PATH NEVER LEAVES THE MACHINE. A worktree path names a person's home
// directory, and this record is read by everyone in the org, so the hosted
// copy is taken through [release.Provenance.ForHosted] rather than trusting
// every caller to strip it. The field does not exist in DeployWorktree at
// all, which is the structural half of the same rule.
//
// Empty fields are omitted rather than sent as "": proto3 JSON treats an
// absent scalar and its zero value identically, so sending them would add
// bytes and say nothing — except for `dirty`, which is sent whenever it is
// true and omitted otherwise for the same reason.
func ProvenanceWireFields(p release.Provenance) map[string]any {
	p = p.ForHosted()
	fields := map[string]any{}
	putNonEmpty(fields, "repo", p.Repo)
	putNonEmpty(fields, "commit", p.Commit)
	putNonEmpty(fields, "branch", p.Branch)
	putNonEmpty(fields, "tag", p.Tag)
	putNonEmpty(fields, "tree", p.Tree)
	putNonEmpty(fields, "forgeVersion", p.ForgeVersion)
	if p.Dirty {
		fields["dirty"] = true
	}
	worktree := map[string]any{}
	putNonEmpty(worktree, "key", p.Worktree.Key)
	putNonEmpty(worktree, "label", p.Worktree.Label)
	putNonEmpty(worktree, "hostId", p.Worktree.Host)
	if len(worktree) > 0 {
		fields["worktree"] = worktree
	}
	if a := p.Attestation; a != nil {
		attestation := map[string]any{}
		putNonEmpty(attestation, "provider", a.Provider)
		putNonEmpty(attestation, "subject", a.Subject)
		putNonEmpty(attestation, "runId", a.RunID)
		if len(attestation) > 0 {
			fields["attestation"] = attestation
		}
	}
	return fields
}

func putNonEmpty(fields map[string]any, key, value string) {
	if value != "" {
		fields[key] = value
	}
}
