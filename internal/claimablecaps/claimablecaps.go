// Package claimablecaps is forge's compiled-in copy of the claimable plan's
// caps, the numbers behind deploy.ClaimableLimitsFallback.
//
// It is a leaf (standard library only) so the scaffold templates can default a
// claimable env without importing pkg/deploy, whose closure (k8s apimachinery,
// OTLP exporters, grpc) would otherwise follow internal/templates into every
// binary that renders a skill or the project memory (pkg/skills).
package claimablecaps

// Limits mirrors deploy.ClaimableLimits field for field: deploy converts one
// into the other, so the two cannot drift without failing to compile. See
// deploy.ClaimableLimits for what each field means.
type Limits struct {
	MaxServices      int
	MaxReplicas      int
	MaxMigrationJobs int

	WorkloadCPUMillicores int64
	WorkloadMemoryBytes   int64

	MaxDatabases               int
	DatabaseInstances          int
	DatabaseDeclaredStorageGiB int32
	DatabaseCPUMillicores      int64
	DatabaseMemoryBytes        int64
	DatabaseStorageBytes       int64
	DatabaseMaxConnections     int

	MaxStaticSites int
}

// Fallback is the claimable plan's caps as of control-plane 2026-10-29, for
// when the control plane cannot be asked. The platform's own answer wins.
func Fallback() Limits {
	return Limits{
		MaxServices: 1, MaxReplicas: 1, MaxMigrationJobs: 1,
		WorkloadCPUMillicores: 125, WorkloadMemoryBytes: 128 << 20,
		MaxDatabases: 1, DatabaseInstances: 1, DatabaseDeclaredStorageGiB: 1,
		DatabaseCPUMillicores: 125, DatabaseMemoryBytes: 128 << 20,
		DatabaseStorageBytes: 256 << 20, DatabaseMaxConnections: 10,
		MaxStaticSites: 1,
	}
}
