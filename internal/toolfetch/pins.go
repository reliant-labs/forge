package toolfetch

import "github.com/reliant-labs/forge/internal/templates"

// Syft is the SBOM generator forge pins (templates.SyftVersion).
var Syft = Tool{
	Name:      "syft",
	Version:   templates.SyftVersion,
	Repo:      "anchore/syft",
	GoPackage: "github.com/anchore/syft/cmd/syft",
	// Copied verbatim from syft_1.57.2_checksums.txt, archives only (the
	// .deb, .rpm and .sbom assets are not what forge installs). Verified:
	// the linux_amd64 archive downloaded from the release hashes to the
	// value below.
	Checksums: map[string]string{
		"syft_1.57.2_darwin_amd64.tar.gz":  "73c6100bc43fbcc1c99aed3428d859c6289c4076be7e296dde7e427324c15908",
		"syft_1.57.2_darwin_arm64.tar.gz":  "38afbcc03a9d9afac2d7572432fd2c435e051e03930582ea90f4710ffb515224",
		"syft_1.57.2_linux_amd64.tar.gz":   "ad92bd3ec1b94a3df01d4d0aec199afa9c42b72e86da436ff5f837914d32706b",
		"syft_1.57.2_linux_arm64.tar.gz":   "8efe605309c98798e1ee0cffdcf8c05ca26819b4cd44c0b03650f1e963e9bdb6",
		"syft_1.57.2_linux_ppc64le.tar.gz": "9475031e14788f78006c63660a657671470cae841d1f221ad84d7f69c20366aa",
		"syft_1.57.2_linux_riscv64.tar.gz": "b1c5ac17db3779f91d01a0583a78c031fa971bdbb5ea3fb1665f78722debff56",
		"syft_1.57.2_linux_s390x.tar.gz":   "87267ac3bcdd7c4545a5715c811cd9a96d76a06a9788b8508a17f5cf1545c280",
		"syft_1.57.2_windows_amd64.zip":    "0a10e03ffd124c304fd7e7491262dba2e5f68eb12378b106d7525146675b2e3a",
		"syft_1.57.2_windows_arm64.zip":    "7f3cace29c2a7d2478de834aaa42663b8dbf82c9e8abc6a0917b437584ea54e1",
	},
}

// Grype is the vulnerability scanner forge pins (templates.GrypeVersion).
var Grype = Tool{
	Name:      "grype",
	Version:   templates.GrypeVersion,
	Repo:      "anchore/grype",
	GoPackage: "github.com/anchore/grype/cmd/grype",
	// Copied verbatim from grype_0.122.1_checksums.txt, archives only.
	// grype publishes no windows/arm64 or linux/riscv64 build: there forge
	// falls back to `go install` (see Fetcher.Ensure). Verified: the
	// linux_amd64 archive downloaded from the release hashes to the value
	// below.
	Checksums: map[string]string{
		"grype_0.122.1_darwin_amd64.tar.gz":  "8063aef11b52e6da342068aa6980e92f40b28849eff990c8375a0a8e6b6dfbc5",
		"grype_0.122.1_darwin_arm64.tar.gz":  "8bbb3c01695d1e954875cf9f7403c5d3cdb652caf763867955fad520e3cd6c00",
		"grype_0.122.1_linux_amd64.tar.gz":   "f1ba7e2917ee1395618de20a98da0aa7c0fb1788927797651047c50502a37538",
		"grype_0.122.1_linux_arm64.tar.gz":   "819e5be3d9516432fedb7b6b56df7306c3a16607606dfc05826eefbb00e471da",
		"grype_0.122.1_linux_ppc64le.tar.gz": "b85faf0e6770b39758a5803e6517d6c6d4e57a5fe8e09f33ce6e678a0ef3fba0",
		"grype_0.122.1_linux_s390x.tar.gz":   "4aff31dc3e75fb4e46d667c3a9b4fc6ce87e57221ebbd484b8aff223e0c6c406",
		"grype_0.122.1_windows_amd64.zip":    "24e19d6378782d2da343b80c632e342bcdd3bd1bf44e415a0ff81cf85ce1043a",
	},
}

// Tools is every tool this package pins, in the order `forge tools pins` and
// `forge doctor` list them.
func Tools() []Tool { return []Tool{Syft, Grype} }
