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

	// SourceHash fingerprints the .proto files this descriptor was extracted
	// from. It exists because the descriptor is a DERIVED CACHE of state that
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
	// inverts it — see DescriptorSourceHash and loadDescriptor.
	//
	// Empty means "written by a forge that predates the stamp": treated as
	// stale, because an unverifiable cache is exactly the thing being fixed.
	SourceHash string `json:"source_hash,omitempty"`
}

// DescriptorSourceHash fingerprints every .proto under <projectDir>/proto by
// path and content. Order-independent by construction (paths are sorted), so
// the same tree always hashes the same regardless of walk order.
//
// Content, not mtime: a checkout, a rebase or a `cp -R` rewrites mtimes
// without changing meaning, and would invalidate a descriptor that is
// perfectly good. Hashing the bytes answers the question actually being
// asked — "was this extracted from THESE protos?"
func DescriptorSourceHash(projectDir string) string {
	type entry struct{ rel, sum string }
	var entries []entry

	root := filepath.Join(projectDir, "proto")
	_ = filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() || filepath.Ext(path) != ".proto" {
			return nil //nolint:nilerr // an unreadable tree contributes nothing
		}
		data, readErr := os.ReadFile(path)
		if readErr != nil {
			return nil //nolint:nilerr // same
		}
		rel, relErr := filepath.Rel(root, path)
		if relErr != nil {
			rel = path
		}
		sum := sha256.Sum256(data)
		entries = append(entries, entry{filepath.ToSlash(rel), hex.EncodeToString(sum[:])})
		return nil
	})
	if len(entries) == 0 {
		return ""
	}

	sort.Slice(entries, func(i, j int) bool { return entries[i].rel < entries[j].rel })
	h := sha256.New()
	for _, e := range entries {
		_, _ = h.Write([]byte(e.rel))
		_, _ = h.Write([]byte{0})
		_, _ = h.Write([]byte(e.sum))
		_, _ = h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil))
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
	if want := DescriptorSourceHash(projectDir); want != "" && desc.SourceHash != "" && desc.SourceHash != want {
		return nil, fmt.Errorf(
			"gen/forge_descriptor.json is stale: it was extracted from different .proto files than the ones on disk.\n" +
				"  It is a DERIVED cache — the protos are the source of truth — so forge refuses to answer from it.\n" +
				"  Fix: reliant forge generate")
	}
	return &desc, nil
}

// ParseServicesFromProtos reads service definitions from the forge descriptor.
// Falls back to empty if the descriptor does not exist yet.
func ParseServicesFromProtos(dir string, projectDir string) ([]ServiceDef, error) {
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

// ParseEntityProtos returns the project's entities. Despite the
// historical name, entities are no longer parsed from proto
// annotations: they are the join of the APPLIED schema (db/migrations
// shadow-applied + introspected) with the service protos' CRUD method
// shapes — see BuildSchemaEntities. Falls back to empty when the
// descriptor or migrations don't exist yet.
func ParseEntityProtos(projectDir string) ([]EntityDef, error) {
	desc, err := loadDescriptor(projectDir)
	if err != nil {
		return nil, err
	}
	if desc == nil {
		noticeMissingDescriptor(projectDir)
		return nil, nil
	}
	return BuildSchemaEntities(projectDir, desc.Services)
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
