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
func RegisterFlags(cmd *cobra.Command, msg proto.Message) error {
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
