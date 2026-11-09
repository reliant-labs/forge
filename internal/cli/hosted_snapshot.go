package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"

	"github.com/reliant-labs/forge/internal/deploytarget"
	"github.com/reliant-labs/forge/pkg/release"
)

const procDeployCapabilities = deploytarget.ProcDeployCapabilities

// requireSnapshotCapabilities is deliberately live and fail-closed. Older
// servers silently discard unknown JSON fields; a successful legacy RPC is not
// evidence that a snapshot intent/seal was stored.
func requireSnapshotCapabilities(ctx context.Context, client cloudCaller) error {
	var caps struct {
		Schemas []string `json:"snapshotSchemaVersions"`
		Seals   bool     `json:"immutableSnapshotSeals"`
		Frozen  bool     `json:"frozenPromotionBundles"`
	}
	if err := client.Call(ctx, procDeployCapabilities, map[string]any{}, &caps); err != nil {
		return fmt.Errorf("snapshot server capability required: %w", err)
	}
	supported := false
	for _, schema := range caps.Schemas {
		supported = supported || schema == release.SnapshotSchema
	}
	if !supported || !caps.Seals || !caps.Frozen {
		return fmt.Errorf("server does not support %s with immutable seals and frozen promotion bundles", release.SnapshotSchema)
	}
	return nil
}

// snapshotFeatureEvidenceSeal is the control plane's snapshot feature for an
// evidence seal: a snapshot recorded on a bundle of a release cut WITHOUT
// snapshot intents, which the promotion shipping that bundle freezes
// (GetDeployCapabilitiesResponse.snapshot_features; control-plane
// deploy.SnapshotFeatureEvidenceSeal).
const snapshotFeatureEvidenceSeal = "evidence-seal"

// evidenceSealSupported reports whether the control plane takes an evidence
// seal: every capability requireSnapshotCapabilities demands, plus
// snapshot_features naming evidence-seal. Unlike requireSnapshotCapabilities
// it is not fail-closed on the deploy: a false (or an error) means the env
// deploy writes its legacy bundle, exactly as before the feature existed. An
// older control plane routes a snapshot on an intent-less release down its
// intent path and refuses it, which is why nothing is sealed unless it says so.
func evidenceSealSupported(ctx context.Context, client cloudCaller) (bool, error) {
	var caps struct {
		Schemas  []string `json:"snapshotSchemaVersions"`
		Seals    bool     `json:"immutableSnapshotSeals"`
		Frozen   bool     `json:"frozenPromotionBundles"`
		Features []string `json:"snapshotFeatures"`
	}
	if err := client.Call(ctx, procDeployCapabilities, map[string]any{}, &caps); err != nil {
		return false, err
	}
	schema, evidence := false, false
	for _, s := range caps.Schemas {
		schema = schema || s == release.SnapshotSchema
	}
	for _, f := range caps.Features {
		evidence = evidence || f == snapshotFeatureEvidenceSeal
	}
	return schema && caps.Seals && caps.Frozen && evidence, nil
}

func snapshotReleaseFromJSON(raw json.RawMessage, version string) (release.Release, error) {
	var r release.Release
	d := json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	if err := d.Decode(&r); err != nil {
		return r, fmt.Errorf("snapshot release: %w", err)
	}
	if err := d.Decode(new(any)); err != io.EOF {
		return r, fmt.Errorf("snapshot release trailing data")
	}
	if r.Version != version {
		return r, fmt.Errorf("snapshot release version does not match envelope")
	}
	if len(r.Catalog) == 0 || len(r.SnapshotIntents) == 0 {
		return r, fmt.Errorf("snapshot release missing catalog or intents")
	}
	if err := r.Validate(); err != nil {
		return r, err
	}
	return r, nil
}
