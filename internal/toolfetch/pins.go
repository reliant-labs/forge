package toolfetch

import "github.com/reliant-labs/forge/internal/templates"

// How these pins were made, and how to remake them on a bump:
//
//   - Checksums: copied verbatim from the release's <tool>_<ver>_checksums.txt,
//     archives only (the .deb, .rpm and .sbom assets are not what forge
//     installs).
//   - BinaryChecksums: every archive in Checksums was downloaded once from
//     https://github.com/<repo>/releases/download/<version>/<asset>, checked
//     against its Checksums value (`sha256sum`, all matched), and the
//     executable member hashed: `tar xzOf <asset> <name> | sha256sum`, or
//     `unzip -p <asset> <name>.exe | sha256sum` for a .zip.
//
// The check at install (the extracted executable must hash to its
// BinaryChecksums value) catches a BinaryChecksums row that disagrees with
// its archive the first time anyone installs that platform, and
// TestPinnedBinaryChecksumsMatchTheirArchives (FORGE_TEST_TOOLFETCH_LIVE=1)
// re-derives all of them from the release.

// Syft is the SBOM generator forge pins (templates.SyftVersion).
var Syft = Tool{
	Name:      "syft",
	Version:   templates.SyftVersion,
	Repo:      "anchore/syft",
	GoPackage: "github.com/anchore/syft/cmd/syft",
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
	BinaryChecksums: map[string]string{
		"syft_1.57.2_darwin_amd64.tar.gz":  "937b74689e65014b1f5720ded9b3bbf6a464c0e3f50f1c162cbaf3373199bd8e",
		"syft_1.57.2_darwin_arm64.tar.gz":  "a55d46242c69903c31c10fdceb4579a9f712280b6994001949f8ef634558a99f",
		"syft_1.57.2_linux_amd64.tar.gz":   "17cc40c1e40dbfdeb8c84b6d9928909bceb793f22e8a8023ef408625f1d96ec5",
		"syft_1.57.2_linux_arm64.tar.gz":   "2bbe674996bbe831dcc68fa50b0c5e68acd728c5d9da7cc68d88713a9ef6b748",
		"syft_1.57.2_linux_ppc64le.tar.gz": "a3ca5d637bfbbeab5950ba329a102dae6a4b33b637fd0b66e7509556ddf0755f",
		"syft_1.57.2_linux_riscv64.tar.gz": "15415fcac4d1a52dbb8fe1ad3925331eb0136b58514a7578670e5407840a526a",
		"syft_1.57.2_linux_s390x.tar.gz":   "9442f0ba30d06d1ae9e6809e764cc595310b43bef232283a09f385c2d2914782",
		"syft_1.57.2_windows_amd64.zip":    "5c9d2ba5affae29c3c5ba5796ad28fc72780189fafcb893544325902a298b17e",
		"syft_1.57.2_windows_arm64.zip":    "fc0dc146da865c78c3be317bbdfaa2d28e90dd80594c39cccd56ec5603c82621",
	},
}

// Grype is the vulnerability scanner forge pins (templates.GrypeVersion).
// grype publishes no windows/arm64 or linux/riscv64 build: there forge falls
// back to `go install` (see Fetcher.Ensure).
var Grype = Tool{
	Name:      "grype",
	Version:   templates.GrypeVersion,
	Repo:      "anchore/grype",
	GoPackage: "github.com/anchore/grype/cmd/grype",
	Checksums: map[string]string{
		"grype_0.122.1_darwin_amd64.tar.gz":  "8063aef11b52e6da342068aa6980e92f40b28849eff990c8375a0a8e6b6dfbc5",
		"grype_0.122.1_darwin_arm64.tar.gz":  "8bbb3c01695d1e954875cf9f7403c5d3cdb652caf763867955fad520e3cd6c00",
		"grype_0.122.1_linux_amd64.tar.gz":   "f1ba7e2917ee1395618de20a98da0aa7c0fb1788927797651047c50502a37538",
		"grype_0.122.1_linux_arm64.tar.gz":   "819e5be3d9516432fedb7b6b56df7306c3a16607606dfc05826eefbb00e471da",
		"grype_0.122.1_linux_ppc64le.tar.gz": "b85faf0e6770b39758a5803e6517d6c6d4e57a5fe8e09f33ce6e678a0ef3fba0",
		"grype_0.122.1_linux_s390x.tar.gz":   "4aff31dc3e75fb4e46d667c3a9b4fc6ce87e57221ebbd484b8aff223e0c6c406",
		"grype_0.122.1_windows_amd64.zip":    "24e19d6378782d2da343b80c632e342bcdd3bd1bf44e415a0ff81cf85ce1043a",
	},
	BinaryChecksums: map[string]string{
		"grype_0.122.1_darwin_amd64.tar.gz":  "ca82c975f0788c9fa5a882c5eaa4b848e9d2c7c7d28d20519eadfe3cdc9a22f4",
		"grype_0.122.1_darwin_arm64.tar.gz":  "df8effdd7d4069cfd8762516aa37ddc9e5bf026469f4f509daae2ef730c517f1",
		"grype_0.122.1_linux_amd64.tar.gz":   "322a7839c1c9f6c1b6ac0aa6b61a7d94139b7f24780b82a05b63caab56d4286b",
		"grype_0.122.1_linux_arm64.tar.gz":   "bd313717cfe7fa6807fed57942b661b02aa1709c77faaf3079a9b217b3a3d98c",
		"grype_0.122.1_linux_ppc64le.tar.gz": "ed8366874c0adad69486643d9dad9ec74e8750519c8bc56971a63517ec421d21",
		"grype_0.122.1_linux_s390x.tar.gz":   "24aed62d4ef97ce7cc8cb6075af3b19bf7acef6866f3bbd2eb5e32467af76317",
		"grype_0.122.1_windows_amd64.zip":    "663d5d39c88013ef043a159f7ccc3bf6a17a1bb1346edfc2272e2b7e241daf27",
	},
}

// Tools is every tool this package pins, in the order `forge tools pins` and
// `forge doctor` list them.
func Tools() []Tool { return []Tool{Syft, Grype} }
