package serverkit

// compose.go — the INVARIANT half of a forge composition root.
//
// cmd/<bin>/cmd/serve.go is scaffolded ONCE and then owned by the project:
// it is the file where an application author changes the auth posture, the
// interceptor order, the payload caps, the CORS policy, the readiness set
// and the teardown. None of those choices belong to forge.
//
// Everything in THIS file is the other half — the steps that are identical
// in every forge project and that an author has no reason to re-derive:
// building the logger and the mux, running migrations before anything
// mounts, recording mounted services for the completeness gate, scoping the
// proto registry to the project's own services, and adding an
// already-selected worker set. They live here so serve.go can be short
// enough to read, and so improvements to them reach every project without
// anyone re-scaffolding.
//
// The steps are deliberately SEPARATE and individually callable rather than
// one Compose(...) god-function: a composition root that needs to migrate
// after mounting, skip the readiness pool, or wrap the mux has to be able to
// reorder and replace pieces. A step that can only be used in one position
// is not a library, it is a template with extra syntax.

import (
	"database/sql"
	"fmt"
	"log/slog"
	"net/http"

	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/reflect/protoregistry"

	"github.com/reliant-labs/forge/pkg/mountkit/inventory"
)

// Boot performs the two startup steps that must happen before a composition
// root can do anything else, and that every composition root does the same
// way: build the root logger from cfg and install it as slog's default, then
// create the mux services will mount onto.
//
// The logger is built ONCE, here, and threaded through mount time and run
// time by the caller so mount-time and run-time logs agree — serverkit.Run
// will build its own only if Server.Logger is left nil, which is exactly the
// case this exists to avoid.
//
// Boot does NOT normalize cfg. Normalization is the caller's step because
// the caller is what reads Config fields (the payload caps, the migration
// driver) before Run is entered; see Config.Normalize.
func Boot(cfg Config) (*slog.Logger, *http.ServeMux) {
	logger := newLogger(cfg)
	slog.SetDefault(logger)
	return logger, http.NewServeMux()
}

// AutoMigrate runs the project's migration function against a DEDICATED,
// short-lived connection, and is a no-op when cfg.AutoMigrate is false.
//
// The whole ceremony is here — dial, apply the pool tuning, migrate, close —
// because getting it wrong is silent in one direction and fatal in the
// other. Callers invoke it BEFORE constructing components and mounting
// services: a service that starts serving against a schema its migrations
// have not reached yet answers requests with errors that look like bugs.
// That ordering is the caller's to keep (it is a line position in serve.go),
// but everything inside it is not.
//
// The connection is closed on every path, including the migration-failure
// path, so a failed boot does not leak a pool into a crash loop. It is
// deliberately NOT the serving pool: migrations run once, at boot, and hold
// locks the serving pool must not be waiting behind.
//
// migrate is the project's own generated pkg/app.AutoMigrate. Passing it in
// (rather than serverkit reaching for it) is what keeps serverkit free of
// any compile-time dependency on the project.
func AutoMigrate(cfg Config, logger *slog.Logger, migrate func(*sql.DB, *slog.Logger) error) error {
	if !cfg.AutoMigrate {
		return nil
	}
	if cfg.DatabaseURL == "" {
		return fmt.Errorf("auto-migrate is enabled but DatabaseURL is not set")
	}
	// Normalize defensively: DBDriver reaches sql.Open by NAME, and a zero
	// value there is `unknown driver ""` rather than the intended default.
	// Normalize is idempotent, so a caller that already normalized pays
	// nothing for the guarantee.
	cfg.Normalize()

	db, err := sql.Open(cfg.DBDriver, cfg.DatabaseURL)
	if err != nil {
		return fmt.Errorf("failed to connect to database for migration: %w", err)
	}
	defer func() { _ = db.Close() }()

	ApplyDBPoolTuning(db, cfg.DBPoolTuning)
	if err := migrate(db, logger); err != nil {
		return fmt.Errorf("auto-migration failed: %w", err)
	}
	if logger != nil {
		logger.Info("db migration completed")
	}
	return nil
}

// RecordMounted records every Connect path a typed mount returned, so the
// boot-time completeness gate can tell mounted services from declared ones.
//
// It is the loop that always follows a mount call. Keeping it as a step
// rather than folding it into the mount itself matters: a composition root
// mounts through its OWN typed Mount<Svc> methods (compile-time selection,
// no string lookup), so serverkit never sees the mount happen and cannot
// record the result on the caller's behalf.
func (s *Server) RecordMounted(connectPaths []string) {
	for _, p := range connectPaths {
		s.Mounted(p)
	}
}

// AddWorkers registers an already-selected worker set for supervision.
//
// Selection happened above serverkit — a per-worker subcommand hands a
// one-element slice, the all-services command hands its full set. serverkit
// supervises exactly what it is given and never filters by name.
func (s *Server) AddWorkers(workers []Worker) {
	for _, w := range workers {
		s.AddWorker(w)
	}
}

// RequireComplete is the boot-time mount-completeness gate, scoped to the
// project's OWN declared services: every service in inv must have been
// recorded as mounted, or boot fails.
//
// It fuses ProjectServiceFiles with RequireMounted because the two are only
// ever correct together, and the failure mode of splitting them is a
// half-wired server that boots green. A composition root that deliberately
// mounts a SUBSET (a per-service subcommand) simply does not call this.
func (s *Server) RequireComplete(inv []inventory.ComponentInfo) error {
	files, err := ProjectServiceFiles(inv)
	if err != nil {
		return fmt.Errorf("server completeness check: scoping project descriptors: %w", err)
	}
	if err := s.RequireMounted(files); err != nil {
		return fmt.Errorf("server completeness check: %w", err)
	}
	return nil
}

// ProjectServiceFiles builds a *protoregistry.Files scoped to the PROJECT's
// own declared services, for the RequireMounted completeness gate.
//
// It looks each inventory ConnectPath (a fully-qualified Connect service
// name) up in protoregistry.GlobalFiles and registers ONLY those services'
// parent FileDescriptors. GlobalFiles is not usable directly: it also
// carries framework and transitively-imported proto files this binary does
// not serve — grpc.health.v1.Health, which serverkit mounts itself, and the
// OTel collector services the SDK pulls in — so a gate reading it would
// demand mounts for services that are not the project's to mount.
//
// A missing descriptor is a hard error: the inventory and the generated
// packages must agree, and a disagreement means the gate would be checking
// something other than what the binary serves.
func ProjectServiceFiles(inv []inventory.ComponentInfo) (*protoregistry.Files, error) {
	files := &protoregistry.Files{}
	seen := make(map[protoreflect.FullName]struct{})
	for _, info := range inv {
		desc, err := protoregistry.GlobalFiles.FindDescriptorByName(protoreflect.FullName(info.ConnectPath))
		if err != nil {
			return nil, fmt.Errorf("service %q (%s) not found in global proto registry: %w", info.Name, info.ConnectPath, err)
		}
		fd := desc.ParentFile()
		if _, ok := seen[fd.FullName()]; ok {
			continue
		}
		seen[fd.FullName()] = struct{}{}
		if err := files.RegisterFile(fd); err != nil {
			return nil, fmt.Errorf("registering file for service %q: %w", info.Name, err)
		}
	}
	return files, nil
}
