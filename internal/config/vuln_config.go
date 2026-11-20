package config

// VulnConfig is the `vuln:` block: how forge keeps the SBOM it records for
// each release artifact. Every key is optional, and an absent block is the
// default — the SBOM goes everywhere it can.
type VulnConfig struct {
	// AttachSBOM says whether the release cut attaches each OCI artifact's
	// SBOM (an image, a hosted static site) to it in its registry, as an OCI
	// 1.1 referrer (with the `sha256-<hex>` tag fallback where the registry
	// has no referrers API). Unset means true.
	//
	// `false` turns attachment off — for a registry that refuses unknown
	// artifact types, or one whose cleanup policy deletes untagged manifests
	// (a referrer is one). The SBOM is still recorded: the release keeps its
	// digest, and the machine ledger (and a control plane, when the env
	// records on one) keeps its bytes.
	//
	//	vuln:
	//	  attach_sbom: false
	AttachSBOM *bool `yaml:"attach_sbom,omitempty"`
}

// AttachSBOMEnabled reports whether OCI artifacts get their SBOM attached as
// a referrer: true unless forge.yaml says `vuln.attach_sbom: false`.
func (v VulnConfig) AttachSBOMEnabled() bool {
	return v.AttachSBOM == nil || *v.AttachSBOM
}
