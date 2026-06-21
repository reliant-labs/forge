package config

// Annotation-driven semantics + the cobra-facing public API.
//
// This file layers the spec'd public surface — RegisterFlags(cmd, msg),
// Load(cmd, msg), and the typed Load[T] convenience — over the
// descriptor-driven primitives in loader.go (RegisterFlagsFor / LoadInto),
// and adds the role-annotation helpers (Mode, DevAuthBypass).
//
// The defining property: NONE of these helpers match on a field's NAME.
// Mode/DevAuthBypass key off the field tagged role=CONFIG_FIELD_ROLE_MODE.
// Renaming that field changes nothing; naming an UNannotated field
// "environment" does NOT make it the mode field. Behavior follows the
// annotation, not the identifier.

import (
	"fmt"
	"os"
	"strings"

	"github.com/spf13/cobra"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"

	"github.com/reliant-labs/forge/pkg/forgepb"
)

// RegisterFlags registers one cobra flag per config-bound field of msg that
// carries a non-empty flag annotation, using the annotated name, default,
// and description. Sensitive fields are skipped (env/Secret only). It is the
// cobra-facing form of RegisterFlagsFor and is generic over ANY config
// message — no per-field codegen.
//
// It ALSO registers a persistent --config <path> flag (ConfigFlag) — always,
// for every config message. This is the first-class config-FILE layer's entry
// point: it is on, not dormant. Load resolves the file from this flag (or the
// FORGE_CONFIG env var) and unmarshals it as the layer between defaults and
// env. Registering it persistent means it is also visible to subcommands. The
// flag is registered idempotently so re-registering on the same command (or a
// parent that already has it) is safe.
func RegisterFlags(cmd *cobra.Command, msg proto.Message) error {
	if cmd.PersistentFlags().Lookup(ConfigFlag) == nil && cmd.Flags().Lookup(ConfigFlag) == nil {
		cmd.PersistentFlags().String(ConfigFlag, "",
			fmt.Sprintf("path to a config file (.json/.yaml/.yml); also settable via %s. "+
				"Precedence: defaults < file < env < flags", ConfigPathEnv))
	}
	return RegisterFlagsFor(cmd.Flags(), msg)
}

// Load populates msg in place from cobra flags > environment variables >
// proto defaults, for every field carrying a (forge.v1.config) option.
// It is the cobra-facing alias of LoadInto. cmd may be nil (env + defaults
// only). Sensitive fields resolve from env / Secret mount only.
func Load(cmd *cobra.Command, msg proto.Message) error {
	return LoadInto(cmd, msg)
}

// LoadTyped is the generic convenience: it allocates a fresh T, loads it,
// and returns it typed. T must be a pointer proto message type (e.g.
// *configv1.AppConfig). Usage:
//
//	cfg, err := config.LoadTyped[*configv1.AppConfig](cmd)
//
// It mirrors the generated Load(cmd) (*Config, error) signature shape so a
// project can drop the generated loader and call this directly.
func LoadTyped[T proto.Message](cmd *cobra.Command) (T, error) {
	var zero T
	msg := zero.ProtoReflect().New().Interface()
	if err := LoadInto(cmd, msg); err != nil {
		return zero, err
	}
	return msg.(T), nil
}

// RuntimeMode is the typed runtime mode. The zero value is ModeProduction:
// a message with no role=MODE field (or one whose mode field is empty)
// always reports production — dev permissiveness is never the default.
type RuntimeMode int

const (
	// ModeProduction is the zero value: authz enforced, auth required.
	ModeProduction RuntimeMode = iota
	// ModeDevelopment enables local-dev permissiveness. Never set in
	// deployed environments.
	ModeDevelopment
)

// IsDev reports whether the mode is ModeDevelopment. It gates NON-SECURITY
// dev ergonomics only (permissive CORS, verbose errors, dev DB defaults).
// It MUST NOT gate auth — see DevAuthBypass.
func (m RuntimeMode) IsDev() bool { return m == ModeDevelopment }

// modeFieldValue returns the string value of the field tagged
// role=CONFIG_FIELD_ROLE_MODE on msg, and whether such a field exists. The
// lookup is by ANNOTATION, never by name: a field named "environment"
// without the role is ignored; a role-tagged field named anything is found.
func modeFieldValue(msg proto.Message) (string, bool) {
	m := msg.ProtoReflect()
	fields := m.Descriptor().Fields()
	for i := 0; i < fields.Len(); i++ {
		fd := fields.Get(i)
		opt := roleOptions(fd)
		if opt == nil || opt.GetRole() != forgepb.ConfigFieldRole_CONFIG_FIELD_ROLE_MODE {
			continue
		}
		if fd.Kind() != protoreflect.StringKind {
			continue // mode is a string switch; non-string role fields are ignored
		}
		return m.Get(fd).String(), true
	}
	return "", false
}

// roleOptions reads the (forge.v1.config) extension purely to inspect the
// role tag. Unlike fieldOptions it does NOT require the field to also name a
// binding source — a field may carry only a role. Returns nil when the
// extension is absent.
func roleOptions(fd protoreflect.FieldDescriptor) *forgepb.ConfigFieldOptions {
	opts := fd.Options()
	if opts == nil {
		return nil
	}
	ext := proto.GetExtension(opts, forgepb.E_Config)
	cfg, ok := ext.(*forgepb.ConfigFieldOptions)
	if !ok || cfg == nil {
		return nil
	}
	return cfg
}

// Mode derives the runtime mode from msg's role=MODE field:
// "development"/"dev" (case-insensitive) → ModeDevelopment, anything else
// (including no role field) → ModeProduction. Deriving — rather than caching
// — means a hand-built message in tests can never disagree with its own
// mode value.
func Mode(msg proto.Message) RuntimeMode {
	v, ok := modeFieldValue(msg)
	if !ok {
		return ModeProduction
	}
	switch strings.ToLower(v) {
	case "development", "dev":
		return ModeDevelopment
	}
	return ModeProduction
}

// DevAuthBypass reports whether this server runs with NO real auth: authn
// passthrough + authz allow-all + synthetic dev claims.
//
// It is an EXPLICIT, two-factor opt-in and is NEVER implied by the mode
// field alone: development mode gives dev ergonomics but KEEPS auth
// enforced. To actually bypass auth you must ALSO set AUTH_DEV_MODE=true.
// In production the bypass is impossible — Mode is never dev there.
func DevAuthBypass(msg proto.Message) bool {
	if !Mode(msg).IsDev() {
		return false
	}
	return strings.EqualFold(os.Getenv("AUTH_DEV_MODE"), "true")
}

// Validate runs the cross-field and closed-set config invariants that
// cannot be expressed as per-field defaults, failing fast (before the
// listener binds) on the known misconfiguration classes. It is the
// annotation/TYPE-driven replacement for the deleted name-matched
// validators (CORS-wildcard / TLS-pair / log-format):
//
//   - allowed_values: a string field carrying a closed value set is a
//     string ENUM; a resolved value outside the set is rejected. (True
//     proto enum fields need no check — protoreflect enforces their
//     domain. The empty value is always allowed: an unset, non-required
//     field.)
//   - TLS keypair: the fields tagged role=TLS_CERT and role=TLS_KEY are
//     both-or-neither. Exactly one set is an error (the server would
//     silently serve plaintext).
//   - CORS: a wildcard origin ("*") in the role=CORS_ORIGINS field combined
//     with role=CORS_ALLOW_CREDENTIALS=true is spec-invalid and rejected.
//
// Every check keys off the ANNOTATION, never the field name — renaming a
// field never silently drops its guard. Validate recurses into nested
// config blocks so a block can carry its own allowed_values fields. The
// cross-field TLS/CORS checks operate per message level (the role fields of
// the same message), which is where those pairs naturally live.
func Validate(msg proto.Message) error {
	return validateMessage(msg.ProtoReflect())
}

func validateMessage(m protoreflect.Message) error {
	fields := m.Descriptor().Fields()

	var (
		tlsCert, tlsKey               string
		haveTLSCert, haveTLSKey       bool
		corsOrigins                   string
		corsAllowCreds                bool
		haveCORSOrigins, haveCORSCred bool
	)

	for i := 0; i < fields.Len(); i++ {
		fd := fields.Get(i)

		// Recurse into nested config blocks first.
		if isConfigBlock(fd) {
			if err := validateMessage(m.Get(fd).Message()); err != nil {
				return err
			}
			continue
		}

		opt := roleOptions(fd)
		if opt == nil {
			continue
		}

		// allowed_values closed-set check (string fields only).
		if vals := opt.GetAllowedValues(); len(vals) > 0 && fd.Kind() == protoreflect.StringKind {
			got := m.Get(fd).String()
			if got != "" && !containsStr(vals, got) {
				return fmt.Errorf("config field %s: value %q is not one of the allowed values %v", fd.Name(), got, vals)
			}
		}

		switch opt.GetRole() {
		case forgepb.ConfigFieldRole_CONFIG_FIELD_ROLE_TLS_CERT:
			if fd.Kind() == protoreflect.StringKind {
				tlsCert, haveTLSCert = m.Get(fd).String(), true
			}
		case forgepb.ConfigFieldRole_CONFIG_FIELD_ROLE_TLS_KEY:
			if fd.Kind() == protoreflect.StringKind {
				tlsKey, haveTLSKey = m.Get(fd).String(), true
			}
		case forgepb.ConfigFieldRole_CONFIG_FIELD_ROLE_CORS_ORIGINS:
			if fd.Kind() == protoreflect.StringKind {
				corsOrigins, haveCORSOrigins = m.Get(fd).String(), true
			}
		case forgepb.ConfigFieldRole_CONFIG_FIELD_ROLE_CORS_ALLOW_CREDENTIALS:
			if fd.Kind() == protoreflect.BoolKind {
				corsAllowCreds, haveCORSCred = m.Get(fd).Bool(), true
			}
		}
	}

	// TLS keypair: both-or-neither.
	if haveTLSCert && haveTLSKey {
		certSet := tlsCert != ""
		keySet := tlsKey != ""
		if certSet != keySet {
			return fmt.Errorf("TLS is half-configured: set BOTH the TLS cert and key (role=TLS_CERT/TLS_KEY) or NEITHER — exactly one was provided")
		}
	}

	// CORS: wildcard origin + credentials is spec-invalid.
	if haveCORSOrigins && haveCORSCred && corsAllowCreds && hasWildcardOrigin(corsOrigins) {
		return fmt.Errorf("CORS misconfiguration: a wildcard origin (\"*\") MUST NOT be combined with credentials (role=CORS_ALLOW_CREDENTIALS=true); name explicit origins instead")
	}

	return nil
}

func containsStr(set []string, v string) bool {
	for _, s := range set {
		if s == v {
			return true
		}
	}
	return false
}

// hasWildcardOrigin reports whether the comma-separated origins list
// contains a bare "*" entry.
func hasWildcardOrigin(origins string) bool {
	for _, o := range strings.Split(origins, ",") {
		if strings.TrimSpace(o) == "*" {
			return true
		}
	}
	return false
}
