package config

// Descriptor-driven config loader.
//
// This is THE config loader: forge no longer emits a per-field loader into a
// project. Instead of generating Go code from the config proto, it reads the
// (forge.v1.config) field options off a proto.Message's descriptor at RUNTIME
// and resolves each field with the canonical forge precedence (later layers
// override earlier ones): proto default < config FILE < environment variable <
// explicit CLI flag. A project holds a *configv1.AppConfig (the config object
// IS the proto type) and calls LoadInto/RegisterFlags — the generated
// pkg/config/config.go is a thin shim that aliases Config to the proto type and
// wraps these entrypoints.
//
// The resolution semantics:
//
//   - Precedence: defaults → config file → env (LookupEnv) → flag (only if
//     cmd.Flags().Changed), then a required-field check. The file layer
//     (filelayer.go) loads only when a path is EXPLICITLY given and fails
//     loudly on a missing/invalid explicit path. See LoadInto.
//   - Empty-env handling: a string field treats an explicitly-empty env
//     var ("") as SET; every non-string scalar treats "" as unset, because
//     parsing "" would always error. See allowEmptyEnv.
//   - A malformed value is an error that aborts loading — never a silent
//     fallback to the default.
//   - Durations: a Go duration is recognized ONLY when the proto field is a
//     google.protobuf.Duration message (no name heuristic — declare the
//     field as Duration to get typed-duration behavior). String fields stay
//     strings.

import (
	"fmt"
	"os"
	"strconv"
	"time"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/known/durationpb"

	"github.com/reliant-labs/forge/pkg/forgepb"
)

const durationFullName protoreflect.FullName = "google.protobuf.Duration"

// fieldOptions reads the (forge.v1.config) extension off a field
// descriptor, returning nil when the field carries no such option (those
// fields are skipped entirely — they are not config-bound).
func fieldOptions(fd protoreflect.FieldDescriptor) *forgepb.ConfigFieldOptions {
	opts := fd.Options()
	if opts == nil {
		return nil
	}
	ext := proto.GetExtension(opts, forgepb.E_Config)
	cfg, ok := ext.(*forgepb.ConfigFieldOptions)
	if !ok || cfg == nil {
		return nil
	}
	// proto.GetExtension returns a non-nil zero message when the extension
	// is registered on the message but unset on this field. Treat a fully
	// blank option as "not config-bound" so such a field is skipped, the
	// same way the generator skips fields with no annotation. A field is
	// considered config-bound if it names any binding source.
	if cfg.GetEnvVar() == "" && cfg.GetFlag() == "" && cfg.GetDefaultValue() == "" && !cfg.GetRequired() {
		return nil
	}
	return cfg
}

// isDurationField reports whether a field is a google.protobuf.Duration
// message — the only duration shape the library recognizes (see the
// file-level note on the generated loader's name heuristic).
func isDurationField(fd protoreflect.FieldDescriptor) bool {
	return fd.Kind() == protoreflect.MessageKind &&
		fd.Message() != nil &&
		fd.Message().FullName() == durationFullName
}

// isConfigBlock reports whether a field is a NESTED CONFIG BLOCK: a
// singular message field that is not a google.protobuf.Duration and whose
// message type carries at least one config-bound (or role/allowed_values
// annotated) leaf. These are the component config "blocks" that compose
// onto the root config (e.g. AppConfig.trader → TraderConfig); the loader
// descends into them and populates the sub-message. A repeated/map message
// field is never a block — config blocks are singular composition.
func isConfigBlock(fd protoreflect.FieldDescriptor) bool {
	if fd.Kind() != protoreflect.MessageKind || fd.IsList() || fd.IsMap() {
		return false
	}
	if isDurationField(fd) {
		return false
	}
	md := fd.Message()
	if md == nil {
		return false
	}
	sub := md.Fields()
	for i := 0; i < sub.Len(); i++ {
		if fieldOptions(sub.Get(i)) != nil || roleOptions(sub.Get(i)) != nil {
			return true
		}
	}
	return false
}

// allowEmptyEnv mirrors the generated AllowEmptyEnv: an explicitly-empty
// env var counts as "set" only for plain string scalars. Every other kind
// (numeric, bool, duration message) treats "" as unset because parsing ""
// would always error.
func allowEmptyEnv(fd protoreflect.FieldDescriptor) bool {
	return fd.Kind() == protoreflect.StringKind
}

// RegisterFlagsFor walks msg's fields and registers one cobra/pflag flag
// per field that carries a non-empty (forge.v1.config).flag. The flag's
// type matches the proto field kind (string/int32/int64/bool/float;
// duration messages register as a string flag — LoadInto parses "5s" →
// Duration), and its default is the proto option's DefaultValue.
//
// It is the pflag-level primitive; the cobra-facing RegisterFlags(cmd, msg)
// in semantic.go wraps it. It recurses into nested config blocks.
//
// Fields without a flag (typically secrets sourced only from env / Secret
// mounts) are intentionally skipped — defense-in-depth against shell-history
// / `ps` exposure of credentials.
func RegisterFlagsFor(flags *pflag.FlagSet, msg proto.Message) error {
	return registerFlagsForDesc(flags, msg.ProtoReflect().Descriptor())
}

// registerFlagsForDesc registers flags for a descriptor, recursing into
// nested config-block messages. It walks the DESCRIPTOR (not a live
// message) because flag registration needs no instance — only the field
// shapes and annotations. A nested message field that is itself a config
// block (carries config-bound leaves and is not a google.protobuf.Duration)
// is descended into: its leaves keep their OWN env/flag annotations, so a
// block's flags share the flat namespace exactly as the generated loader
// emitted them. Recursion is unbounded in depth but cycle-free in practice
// (a config proto is a finite tree).
func registerFlagsForDesc(flags *pflag.FlagSet, desc protoreflect.MessageDescriptor) error {
	fields := desc.Fields()
	for i := 0; i < fields.Len(); i++ {
		fd := fields.Get(i)
		if isConfigBlock(fd) {
			if err := registerFlagsForDesc(flags, fd.Message()); err != nil {
				return err
			}
			continue
		}
		opt := fieldOptions(fd)
		if opt == nil || opt.GetFlag() == "" {
			continue
		}
		// Sensitive fields are NEVER exposed as flags — their value must come
		// from an env var / Secret mount, never from shell history or `ps`.
		// This holds even if the field carries a flag annotation by mistake.
		if opt.GetSensitive() {
			continue
		}
		name := opt.GetFlag()
		def := opt.GetDefaultValue()
		desc := opt.GetDescription()

		if isDurationField(fd) {
			flags.String(name, def, desc)
			continue
		}
		switch fd.Kind() {
		case protoreflect.StringKind:
			flags.String(name, def, desc)
		case protoreflect.Int32Kind, protoreflect.Sint32Kind, protoreflect.Sfixed32Kind:
			v, err := parseDefaultInt(def, 32)
			if err != nil {
				return fmt.Errorf("config field %s: invalid int32 default %q: %w", fd.Name(), def, err)
			}
			flags.Int32(name, int32(v), desc)
		case protoreflect.Int64Kind, protoreflect.Sint64Kind, protoreflect.Sfixed64Kind:
			v, err := parseDefaultInt(def, 64)
			if err != nil {
				return fmt.Errorf("config field %s: invalid int64 default %q: %w", fd.Name(), def, err)
			}
			flags.Int64(name, v, desc)
		case protoreflect.Uint32Kind, protoreflect.Fixed32Kind:
			v, err := parseDefaultUint(def, 32)
			if err != nil {
				return fmt.Errorf("config field %s: invalid uint32 default %q: %w", fd.Name(), def, err)
			}
			flags.Uint32(name, uint32(v), desc)
		case protoreflect.Uint64Kind, protoreflect.Fixed64Kind:
			v, err := parseDefaultUint(def, 64)
			if err != nil {
				return fmt.Errorf("config field %s: invalid uint64 default %q: %w", fd.Name(), def, err)
			}
			flags.Uint64(name, v, desc)
		case protoreflect.BoolKind:
			v := false
			if def != "" {
				b, err := strconv.ParseBool(def)
				if err != nil {
					return fmt.Errorf("config field %s: invalid bool default %q: %w", fd.Name(), def, err)
				}
				v = b
			}
			flags.Bool(name, v, desc)
		case protoreflect.FloatKind:
			v, err := parseDefaultFloat(def, 32)
			if err != nil {
				return fmt.Errorf("config field %s: invalid float default %q: %w", fd.Name(), def, err)
			}
			flags.Float32(name, float32(v), desc)
		case protoreflect.DoubleKind:
			v, err := parseDefaultFloat(def, 64)
			if err != nil {
				return fmt.Errorf("config field %s: invalid double default %q: %w", fd.Name(), def, err)
			}
			flags.Float64(name, v, desc)
		default:
			return fmt.Errorf("config field %s: unsupported kind %s for flag registration", fd.Name(), fd.Kind())
		}
	}
	return nil
}

// LoadInto populates msg in place using the full forge config precedence —
// later layers OVERRIDE earlier ones:
//
//		defaults  →  config file  →  environment  →  flags
//		(earliest)                                   (wins)
//
//	  - defaults: every annotated field's proto default (sensitive fields get
//	    none — an inline secret default would be a leak).
//	  - config file: when a path is EXPLICITLY given via the --config flag or
//	    the FORGE_CONFIG env var, the file is proto-native unmarshaled INTO msg,
//	    overlaying the defaults (see filelayer.go). A missing/invalid explicit
//	    file is a LOUD error — never a silent fallback. With no path given this
//	    layer is simply skipped (normal precedence, not a fallback).
//	  - environment: each field's env var overrides the file/default value.
//	  - flags: a flag changed on THIS invocation overrides everything.
//
// It is the runtime, descriptor-driven loader: empty-env handling, required-
// field errors, and per-kind parsing all key off the (forge.v1.config) field
// options. cmd may be nil (file via FORGE_CONFIG + env + defaults only), so
// LoadInto works from non-cobra entrypoints too.
//
// Durations: declare a duration-shaped field as google.protobuf.Duration in
// the config proto — env/flag layers parse the Go-duration string ("5s")
// into the message, and consumers read it with .AsDuration(). (A plain
// string field stays a string; the descriptor has no name heuristic.)
//
// The companion semantics — Mode/DevAuthBypass (role=MODE) and Validate
// (TLS/CORS roles + allowed_values) — live in this package too (semantic.go),
// as FREE FUNCTIONS over the message. There is no parallel generated struct:
// a project holds the proto config type and calls these directly.
func LoadInto(cmd *cobra.Command, msg proto.Message) error {
	m := msg.ProtoReflect()

	// Layer 1: defaults (the base every later layer overlays).
	if err := applyDefaults(m); err != nil {
		return err
	}

	// Layer 2: config file, ONLY when an explicit path is given (--config
	// flag or FORGE_CONFIG env). A missing/invalid explicit file aborts
	// LOUDLY here; no path given means this layer is skipped (not a fallback).
	if path, ok := resolveConfigPath(cmd); ok {
		if err := applyConfigFile(msg, path); err != nil {
			return err
		}
	}

	// Layers 3 + 4: env then flags overlay onto whatever defaults/file set.
	if err := applyEnvAndFlags(cmd, m); err != nil {
		return err
	}

	// Required fields must be satisfied by SOME layer (default/file/env/flag).
	return checkRequired(m)
}

// applyDefaults sets every annotated field to its proto default, recursing
// into nested config blocks. Sensitive fields are skipped — a literal secret
// default would be a leak (their value must come from file/env). A field with
// no default annotation is left at its proto zero value.
func applyDefaults(m protoreflect.Message) error {
	fields := m.Descriptor().Fields()
	for i := 0; i < fields.Len(); i++ {
		fd := fields.Get(i)
		if isConfigBlock(fd) {
			if err := applyDefaults(m.Mutable(fd).Message()); err != nil {
				return err
			}
			continue
		}
		opt := fieldOptions(fd)
		if opt == nil || opt.GetSensitive() || opt.GetDefaultValue() == "" {
			continue
		}
		val, err := parseValue(fd, opt.GetDefaultValue())
		if err != nil {
			return fmt.Errorf("invalid default %q for config field %s: %w", opt.GetDefaultValue(), fd.Name(), err)
		}
		m.Set(fd, val)
	}
	return nil
}

// applyEnvAndFlags overlays the env layer then the flag layer onto m,
// recursing into nested config blocks. For each field the env var (if set,
// honoring the empty-string rule for the field's kind) overrides the current
// value, then a flag changed on THIS invocation overrides that. A field that
// neither env nor flag touches keeps whatever defaults/file set.
func applyEnvAndFlags(cmd *cobra.Command, m protoreflect.Message) error {
	fields := m.Descriptor().Fields()
	for i := 0; i < fields.Len(); i++ {
		fd := fields.Get(i)
		if isConfigBlock(fd) {
			if err := applyEnvAndFlags(cmd, m.Mutable(fd).Message()); err != nil {
				return err
			}
			continue
		}
		opt := fieldOptions(fd)
		if opt == nil {
			continue
		}
		if err := overlayField(cmd, m, fd, opt); err != nil {
			return err
		}
	}
	return nil
}

// overlayField applies the env layer then the flag layer to a single field.
// Each present source parses+sets onto the message; an absent source is a
// no-op that preserves the earlier layer (default/file). Sensitive fields
// take env (Secret mount) but NEVER a flag — no flag is ever registered for
// them, and we defensively skip the flag branch even if one were.
func overlayField(cmd *cobra.Command, m protoreflect.Message, fd protoreflect.FieldDescriptor, opt *forgepb.ConfigFieldOptions) error {
	// Env layer.
	if envVar := opt.GetEnvVar(); envVar != "" {
		if v, present := os.LookupEnv(envVar); present && (allowEmptyEnv(fd) || v != "") {
			val, err := parseValue(fd, v)
			if err != nil {
				return fmt.Errorf("invalid value %q for config field %s (from env %s): %w", v, fd.Name(), envVar, err)
			}
			m.Set(fd, val)
		}
	}

	// Flag layer (wins). Never for sensitive fields.
	flagName := opt.GetFlag()
	if opt.GetSensitive() {
		flagName = ""
	}
	if cmd != nil && flagName != "" && cmd.Flags().Changed(flagName) {
		if f := cmd.Flags().Lookup(flagName); f != nil {
			val, err := parseValue(fd, f.Value.String())
			if err != nil {
				return fmt.Errorf("invalid value %q for config field %s (from flag --%s): %w", f.Value.String(), fd.Name(), flagName, err)
			}
			m.Set(fd, val)
		}
	}
	return nil
}

// checkRequired errors if any required field is still unset after all layers
// have been applied, recursing into nested config blocks. A required field
// that no layer populated fails fast — the same loud guarantee the single-pass
// loader gave.
func checkRequired(m protoreflect.Message) error {
	fields := m.Descriptor().Fields()
	for i := 0; i < fields.Len(); i++ {
		fd := fields.Get(i)
		if isConfigBlock(fd) {
			if err := checkRequired(m.Get(fd).Message()); err != nil {
				return err
			}
			continue
		}
		opt := fieldOptions(fd)
		if opt == nil || !opt.GetRequired() {
			continue
		}
		if fieldIsEmpty(m, fd) {
			if flag := opt.GetFlag(); flag != "" && !opt.GetSensitive() {
				return fmt.Errorf("required config field %s is not set (env: %s, flag: --%s)", fd.Name(), opt.GetEnvVar(), flag)
			}
			return fmt.Errorf("required config field %s is not set (env: %s)", fd.Name(), opt.GetEnvVar())
		}
	}
	return nil
}

// fieldIsEmpty reports whether a scalar/duration field still holds its zero
// value after loading. For a Duration message, an unpopulated message is
// empty; for scalars, the kind's zero is empty. This is the post-load required
// check — a required field must be non-zero from SOME layer.
func fieldIsEmpty(m protoreflect.Message, fd protoreflect.FieldDescriptor) bool {
	if isDurationField(fd) {
		return !m.Has(fd)
	}
	switch fd.Kind() {
	case protoreflect.StringKind:
		return m.Get(fd).String() == ""
	case protoreflect.BytesKind:
		return len(m.Get(fd).Bytes()) == 0
	case protoreflect.Int32Kind, protoreflect.Sint32Kind, protoreflect.Sfixed32Kind,
		protoreflect.Int64Kind, protoreflect.Sint64Kind, protoreflect.Sfixed64Kind:
		return m.Get(fd).Int() == 0
	case protoreflect.Uint32Kind, protoreflect.Fixed32Kind,
		protoreflect.Uint64Kind, protoreflect.Fixed64Kind:
		return m.Get(fd).Uint() == 0
	case protoreflect.BoolKind:
		return !m.Get(fd).Bool()
	case protoreflect.FloatKind, protoreflect.DoubleKind:
		return m.Get(fd).Float() == 0
	default:
		return !m.Has(fd)
	}
}

// parseValue converts a raw string to the protoreflect.Value for fd's
// kind, mirroring the generated parse helpers (parseInt32, parseInt64,
// parseBool, parseFloat32/64, parseGoDuration, parseString). Duration
// messages parse a Go duration string into a google.protobuf.Duration.
func parseValue(fd protoreflect.FieldDescriptor, raw string) (protoreflect.Value, error) {
	if isDurationField(fd) {
		d, err := time.ParseDuration(raw)
		if err != nil {
			return protoreflect.Value{}, err
		}
		return protoreflect.ValueOfMessage(durationpb.New(d).ProtoReflect()), nil
	}

	switch fd.Kind() {
	case protoreflect.StringKind:
		return protoreflect.ValueOfString(raw), nil
	case protoreflect.BytesKind:
		return protoreflect.ValueOfBytes([]byte(raw)), nil
	case protoreflect.Int32Kind, protoreflect.Sint32Kind, protoreflect.Sfixed32Kind:
		v, err := strconv.ParseInt(raw, 10, 32)
		if err != nil {
			return protoreflect.Value{}, err
		}
		return protoreflect.ValueOfInt32(int32(v)), nil
	case protoreflect.Int64Kind, protoreflect.Sint64Kind, protoreflect.Sfixed64Kind:
		v, err := strconv.ParseInt(raw, 10, 64)
		if err != nil {
			return protoreflect.Value{}, err
		}
		return protoreflect.ValueOfInt64(v), nil
	case protoreflect.Uint32Kind, protoreflect.Fixed32Kind:
		v, err := strconv.ParseUint(raw, 10, 32)
		if err != nil {
			return protoreflect.Value{}, err
		}
		return protoreflect.ValueOfUint32(uint32(v)), nil
	case protoreflect.Uint64Kind, protoreflect.Fixed64Kind:
		v, err := strconv.ParseUint(raw, 10, 64)
		if err != nil {
			return protoreflect.Value{}, err
		}
		return protoreflect.ValueOfUint64(v), nil
	case protoreflect.BoolKind:
		v, err := strconv.ParseBool(raw)
		if err != nil {
			return protoreflect.Value{}, err
		}
		return protoreflect.ValueOfBool(v), nil
	case protoreflect.FloatKind:
		v, err := strconv.ParseFloat(raw, 32)
		if err != nil {
			return protoreflect.Value{}, err
		}
		return protoreflect.ValueOfFloat32(float32(v)), nil
	case protoreflect.DoubleKind:
		v, err := strconv.ParseFloat(raw, 64)
		if err != nil {
			return protoreflect.Value{}, err
		}
		return protoreflect.ValueOfFloat64(v), nil
	default:
		return protoreflect.Value{}, fmt.Errorf("unsupported field kind %s", fd.Kind())
	}
}

// parseDefaultInt parses a flag default, treating "" as 0 (the proto zero)
// so a field without a default registers a zero-valued flag.
func parseDefaultInt(s string, bits int) (int64, error) {
	if s == "" {
		return 0, nil
	}
	return strconv.ParseInt(s, 10, bits)
}

func parseDefaultUint(s string, bits int) (uint64, error) {
	if s == "" {
		return 0, nil
	}
	return strconv.ParseUint(s, 10, bits)
}

func parseDefaultFloat(s string, bits int) (float64, error) {
	if s == "" {
		return 0, nil
	}
	return strconv.ParseFloat(s, bits)
}
