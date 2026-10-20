package codegen

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/reliant-labs/forge/internal/config"
)

// ForgeDescriptor is the JSON structure written by protoc-gen-forge --mode=descriptor.
type ForgeDescriptor struct {
	Services []ServiceDef    `json:"services"`
	Configs  []ConfigMessage `json:"configs"`

	// Sources fingerprints the .proto files this descriptor was extracted
	// from: project-relative path (under proto/) -> sha256 of the content.
	// It exists because the descriptor is a DERIVED CACHE of state that
	// already lives in the protos, and a cache that cannot detect its own
	// staleness does not degrade — it LIES.
	//
	// Measured: renaming one RPC in the descriptor, touching no .proto, made
	// `forge project graph` report an RPC that existed in no proto file and
	// omit one that existed in six places. Nothing warned, because loading
	// was read-file-and-unmarshal with no validation of any kind.
	//
	// A missing descriptor was always handled gracefully; a WRONG one was
	// trusted completely. That asymmetry is backwards, and this field is what
	// inverts it — see DescriptorSources and loadDescriptor.
	//
	// ONE ENTRY PER PROTO, not one digest of all of them. The descriptor is
	// committed, and a single rolled-up digest changes whenever ANY proto
	// does: two branches that edit different protos both rewrote it, git
	// could only keep one side's, and every such rebase left a stale value
	// behind that needed a regenerate and an amend. A map is merged key by
	// key by the merge driver (cli.mergeDescriptorSources), so branches that
	// touched different protos merge to exactly what a generate over the
	// merged protos writes.
	//
	// Empty means "written by a forge that predates the stamp": treated as
	// unverifiable, never as stale-and-refused.
	Sources map[string]string `json:"sources,omitempty"`
}

// DescriptorSources fingerprints every .proto under <projectDir>/proto by
// path and content. Keyed by slash-separated path relative to the project
// root, so the same tree always yields the same map regardless of walk order
// or operating system.
//
// Content, not mtime: a checkout, a rebase or a `cp -R` rewrites mtimes
// without changing meaning, and would invalidate a descriptor that is
// perfectly good. Hashing the bytes answers the question actually being
// asked — "was this extracted from THESE protos?"
//
// nil when there are no protos: nothing to fingerprint, so nothing to enforce.
func DescriptorSources(projectDir string) map[string]string {
	root := filepath.Join(projectDir, "proto")
	var sources map[string]string
	_ = filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() || filepath.Ext(path) != ".proto" {
			return nil //nolint:nilerr // an unreadable tree contributes nothing
		}
		data, readErr := os.ReadFile(path)
		if readErr != nil {
			return nil //nolint:nilerr // same
		}
		rel, relErr := filepath.Rel(projectDir, path)
		if relErr != nil {
			rel = path
		}
		sum := sha256.Sum256(data)
		if sources == nil {
			sources = map[string]string{}
		}
		sources[filepath.ToSlash(rel)] = hex.EncodeToString(sum[:])
		return nil
	})
	return sources
}

// StaleSources compares a descriptor's stamp with the protos on disk. stamped
// is false when the descriptor carries no stamp or there are no protos to
// compare with, which nothing can be enforced against. Otherwise changed
// lists, sorted, the protos whose content differs, was added, or was removed
// since the descriptor was extracted; empty means it is current.
func StaleSources(projectDir string, stamp map[string]string) (changed []string, stamped bool) {
	want := DescriptorSources(projectDir)
	if len(stamp) == 0 || len(want) == 0 {
		return nil, false
	}
	for path, sum := range want {
		if stamp[path] != sum {
			changed = append(changed, path)
		}
	}
	for path := range stamp {
		if _, ok := want[path]; !ok {
			changed = append(changed, path)
		}
	}
	sort.Strings(changed)
	return changed, true
}

// StaleDescriptorError is the refusal for a descriptor that does not match
// the protos on disk, naming the protos that differ.
func StaleDescriptorError(changed []string) error {
	const shown = 5
	list := strings.Join(changed, ", ")
	if len(changed) > shown {
		list = strings.Join(changed[:shown], ", ") + fmt.Sprintf(", … (%d more)", len(changed)-shown)
	}
	return fmt.Errorf(
		"gen/forge_descriptor.json is stale: it was extracted from different .proto files than the ones on disk (%s).\n"+
			"  It is a DERIVED cache — the protos are the source of truth — so forge refuses to answer from it.\n"+
			"  Fix: reliant forge generate", list)
}

// noticeMissingDescriptor reports the absent gen/forge_descriptor.json —
// but ONLY for a project that actually declares a service in a .proto.
//
// A missing descriptor is a normal, permanent state for a project with no
// service protos (a CLI, a library): the pipeline skips descriptor
// extraction when features.codegen is off or nothing declares a service,
// so "run 'forge generate' with protoc-gen-forge" names a step that will
// never produce the file. Printing it unconditionally made an
// unactionable line the most frequent output of a healthy tree — forge's
// own repo emitted it two to three times per command. Where the project
// DOES declare a service, the descriptor is genuinely pending and the
// notice is real.
func noticeMissingDescriptor(projectDir string) {
	if !protoSourceDeclaresService(projectDir) {
		return
	}
	log.Println("Info: forge_descriptor.json not found — run 'forge generate' with protoc-gen-forge to produce it")
}

// protoSourceDeclaresService reports whether any .proto under
// <projectDir>/proto declares a service. Cheap line scan, matching the
// pipeline's own gate (generate_helpers.go's protoSourceHasService):
// the first token on a line must be the `service` keyword, so prose in
// doc comments and `service`-suffixed message names do not match.
func protoSourceDeclaresService(projectDir string) bool {
	found := false
	_ = filepath.Walk(filepath.Join(projectDir, "proto"), func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() || filepath.Ext(path) != ".proto" {
			return nil //nolint:nilerr // an unreadable tree simply declares no service
		}
		data, readErr := os.ReadFile(path)
		if readErr != nil {
			return nil
		}
		for _, line := range strings.Split(string(data), "\n") {
			if fields := strings.Fields(line); len(fields) > 0 && fields[0] == "service" {
				found = true
				return filepath.SkipAll
			}
		}
		return nil
	})
	return found
}

// loadDescriptor reads and parses the forge_descriptor.json file from the gen/ directory.
func loadDescriptor(projectDir string) (*ForgeDescriptor, error) {
	descPath := filepath.Join(projectDir, "gen", "forge_descriptor.json")
	data, err := os.ReadFile(descPath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil // No descriptor yet — not an error
		}
		return nil, fmt.Errorf("read forge descriptor: %w", err)
	}

	var desc ForgeDescriptor
	if err := json.Unmarshal(data, &desc); err != nil {
		return nil, fmt.Errorf("parse forge descriptor: %w", err)
	}

	// REFUSE a descriptor that does not match the protos on disk. Warning
	// would not do: every caller here feeds codegen or an inspection command,
	// and each would carry on emitting confident, wrong answers from a cache
	// nobody re-derived. A hard error names the one command that fixes it.
	//
	// Only a STAMPED descriptor is checked. An unstamped one is either a
	// pre-guard file (the next generate stamps it) or a hand-built fixture,
	// and refusing those would break every caller that constructs a
	// descriptor directly without a proto tree to hash. The drift this
	// guards against is a stamped descriptor whose protos then changed —
	// which is the sequence that actually happens.
	if changed, stamped := StaleSources(projectDir, desc.Sources); stamped && len(changed) > 0 {
		return nil, StaleDescriptorError(changed)
	}
	return &desc, nil
}

// ParseServicesFromProtos reads the services forge MANAGES from the forge
// descriptor: every declared service except those marked
// `// forge:unmanaged`. Falls back to empty if the descriptor does not exist
// yet.
//
// This is the set every service-scoped consumer reads — handler scaffolds,
// CRUD, mocks, composition and mounts, cmd subcommands, frontend hooks and
// pages, audit, doctor, scaffold. Filtering HERE rather than at each consumer
// is the point: a consumer that forgot to filter would scaffold stubs for
// handlers that live elsewhere, which is the failure the marker exists to
// prevent. A consumer that genuinely needs the whole wire contract asks for
// it by name with ParseDeclaredServices.
func ParseServicesFromProtos(dir string, projectDir string) ([]ServiceDef, error) {
	defs, err := ParseDeclaredServices(projectDir)
	if err != nil {
		return nil, err
	}
	return ManagedServices(defs), nil
}

// ParseDeclaredServices reads EVERY service the protos declare, managed or
// not, with ModulePath set from go.mod. Falls back to empty if the
// descriptor does not exist yet.
//
// Only for consumers that project the API contract rather than forge's
// handler model — the open-procedure set (auth_required describes the rpc,
// whoever implements it), "does this project declare services at all", and
// the unmanaged-service listing in `forge project audit`. Everything else
// reads ParseServicesFromProtos.
func ParseDeclaredServices(projectDir string) ([]ServiceDef, error) {
	desc, err := loadDescriptor(projectDir)
	if err != nil {
		return nil, err
	}
	if desc == nil {
		noticeMissingDescriptor(projectDir)
		return nil, nil
	}

	// Set ModulePath on each service from the project's go.mod
	modulePath, modErr := GetModulePath(projectDir)
	if modErr == nil {
		for i := range desc.Services {
			desc.Services[i].ModulePath = modulePath
		}
	}

	return desc.Services, nil
}

// ManagedServices returns the services in defs that forge manages — those
// without a `// forge:unmanaged` marker — in their original order.
func ManagedServices(defs []ServiceDef) []ServiceDef {
	var out []ServiceDef
	for _, d := range defs {
		if !d.Unmanaged {
			out = append(out, d)
		}
	}
	return out
}

// UnmanagedServices returns the services in defs marked `// forge:unmanaged`,
// in their original order.
func UnmanagedServices(defs []ServiceDef) []ServiceDef {
	var out []ServiceDef
	for _, d := range defs {
		if d.Unmanaged {
			out = append(out, d)
		}
	}
	return out
}

// ParseEntityProtos returns the project's entities. Despite the
// historical name, entities are no longer parsed from proto
// annotations: they are the join of the APPLIED schema (db/migrations
// shadow-applied + introspected) with the service protos' CRUD method
// shapes — see BuildSchemaEntities. Falls back to empty when the
// descriptor or migrations don't exist yet.
//
// Only MANAGED services' CRUD rpcs make an entity. A CRUD-shaped rpc on a
// `// forge:unmanaged` service is hand-implemented elsewhere; projecting an
// entity from it would wire generic CRUD, pages and seeds over business
// logic forge does not own. The entity appears when the service is adopted.
func ParseEntityProtos(projectDir string) ([]EntityDef, error) {
	desc, err := loadDescriptor(projectDir)
	if err != nil {
		return nil, err
	}
	if desc == nil {
		noticeMissingDescriptor(projectDir)
		return nil, nil
	}
	return BuildSchemaEntities(projectDir, ManagedServices(desc.Services))
}

// ParseConfigProto reads config messages from the forge descriptor,
// filtering to those from a specific proto file path.
func ParseConfigProto(protoPath string) ([]ConfigMessage, error) {
	// Infer project dir from the proto path
	projectDir := protoPath
	for {
		parent := filepath.Dir(projectDir)
		if parent == projectDir {
			break
		}
		projectDir = parent
		if _, err := os.Stat(filepath.Join(projectDir, "go.mod")); err == nil {
			break
		}
	}

	desc, err := loadDescriptor(projectDir)
	if err != nil {
		return nil, err
	}
	if desc == nil {
		return nil, nil
	}
	return desc.Configs, nil
}

// ParseConfigProtosFromDir reads all config messages from the forge descriptor.
// Falls back to empty if the descriptor does not exist yet.
func ParseConfigProtosFromDir(dir string) ([]ConfigMessage, error) {
	// Walk up from dir to find the project root (contains go.mod)
	projectDir := dir
	for {
		if _, err := os.Stat(filepath.Join(projectDir, "go.mod")); err == nil {
			break
		}
		parent := filepath.Dir(projectDir)
		if parent == projectDir {
			// Couldn't find go.mod — use the original dir
			projectDir = dir
			break
		}
		projectDir = parent
	}

	desc, err := loadDescriptor(projectDir)
	if err != nil {
		return nil, err
	}
	if desc == nil {
		noticeMissingDescriptor(projectDir)
		return nil, nil
	}
	return desc.Configs, nil
}

// GetModulePath reads the module path from go.mod in the given directory.
func GetModulePath(dir string) (string, error) {
	data, err := os.ReadFile(filepath.Join(dir, "go.mod"))
	if err != nil {
		return "", err
	}

	lines := strings.Split(string(data), "\n")
	for _, line := range lines {
		if strings.HasPrefix(strings.TrimSpace(line), "module ") {
			return strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(line), "module ")), nil
		}
	}

	return "", fmt.Errorf("module directive not found in go.mod")
}

// projectAPIRESTEnabled reports whether the project at projectDir declares
// `api.rest: true`. The bootstrap generator uses it to wrap the Connect mux
// with a vanguard REST transcoder, and the CRUD-gen pass uses it to emit
// `google.api.http` annotations on standard CRUD RPCs.
//
// Best-effort by design: an unreadable or invalid forge.yaml resolves to
// false (no REST) so this is safe to call from any project shape, including
// the initial scaffold pass before forge.yaml grows an `api:` block. It goes
// through the canonical loader rather than scanning lines, so every YAML
// spelling of the key — flow style, quoted scalar, anchor — reads the same
// as it does everywhere else in forge.
func projectAPIRESTEnabled(projectDir string) bool {
	cfg, err := config.LoadProjectDir(projectDir)
	if err != nil {
		return false
	}
	return cfg.API.REST
}
