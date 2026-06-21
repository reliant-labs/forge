package config

// Descriptor-driven config loader.
//
// This is the library form of the loader forge GENERATES into a project's
// pkg/config/config.go. Instead of emitting per-field Go code from the
// config proto, it reads the (forge.v1.config) field options off a
// proto.Message's descriptor at RUNTIME and resolves each field with the
// canonical forge precedence: explicit CLI flag > environment variable >
// proto default. A project can hold a *configv1.AppConfig (or any proto
// message whose fields carry the option) and call LoadInto/RegisterFlags
// rather than carrying ~300 lines of generated loader.
//
// The semantics here MIRROR internal/templates/project/config.go.tmpl +
// internal/codegen/config_gen.go exactly where they translate cleanly:
//
//   - Precedence: flag (only if cmd.Flags().Changed) > env (LookupEnv) >
//     default > required-error > leave-zero. See resolveRaw.
//   - Empty-env handling: a string field treats an explicitly-empty env
//     var ("") as SET (the generated AllowEmptyEnv=true for non-duration
//     strings); every non-string scalar treats "" as unset, because
//     parsing "" would always error. See allowEmptyEnv.
//   - A malformed value is an error that aborts loading — never a silent
//     fallback to the default.
//
// The one behavior that does NOT translate: the generated loader detects
// "duration" string fields by Go-name / default-value heuristic
// (isDurationField) and stores them as time.Duration. A descriptor has no
// such heuristic, so here a Go duration is ONLY recognized when the proto
// field is genuinely a google.protobuf.Duration message. String fields
// stay strings. See the package doc on LoadInto for the migration note.

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
// It is named RegisterFlagsFor (not RegisterFlags) because the package
// already exports a stub RegisterFlags(*cobra.Command) for the field-less
// baseline Config; the two coexist. This is the descriptor-driven form a
// project adopts once it has a config proto message.
//
// Fields without a flag (typically secrets sourced only from env / Secret
// mounts) are intentionally skipped — the same defense-in-depth the
// generated RegisterFlags applies.
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

// LoadInto populates msg in place from cobra flags > environment variables
// > proto defaults, for every field carrying a (forge.v1.config) option.
// It is the runtime, descriptor-driven equivalent of the generated Load:
// the precedence, empty-env handling, required-field errors, and per-kind
// parsing all match config.go.tmpl. cmd may be nil (env + defaults only),
// so LoadInto works from non-cobra entrypoints too.
//
// Migration note for consumers replacing the generated loader: the
// generated Config exposes time.Duration fields for several STRING proto
// fields (pre_stop_delay, shutdown_timeout, db_conn_max_idle_time,
// db_conn_max_lifetime) via a Go-name heuristic the descriptor cannot
// reproduce. To get the same typed-duration behavior from this library,
// those fields must be declared as google.protobuf.Duration in the config
// proto. The generated Validate (CORS/TLS/log-format cross-field checks)
// and the Mode()/DevAuthBypass() helpers are NOT part of this library —
// they remain the consumer's responsibility.
func LoadInto(cmd *cobra.Command, msg proto.Message) error {
	return loadIntoMessage(cmd, msg.ProtoReflect())
}

// loadIntoMessage resolves every config-bound leaf of m, recursing into
// nested config blocks. A block field is allocated with m.Mutable (so the
// sub-message exists even when no leaf is set) and then loaded the same way
// — its leaves carry their own annotations, so block leaves get the exact
// flag>env>default treatment root leaves do.
func loadIntoMessage(cmd *cobra.Command, m protoreflect.Message) error {
	fields := m.Descriptor().Fields()
	for i := 0; i < fields.Len(); i++ {
		fd := fields.Get(i)
		if isConfigBlock(fd) {
			if err := loadIntoMessage(cmd, m.Mutable(fd).Message()); err != nil {
				return err
			}
			continue
		}
		opt := fieldOptions(fd)
		if opt == nil {
			continue
		}
		if err := loadField(cmd, m, fd, opt); err != nil {
			return err
		}
	}
	return nil
}

// loadField resolves and sets a single config field. It first applies the
// flag > env > default precedence to obtain a raw string (resolveRaw),
// then parses+sets it onto the message via protoreflect. An unset,
// non-required field is left at its proto zero value (matching the
// generated loadField, which returns the type zero in that case).
func loadField(cmd *cobra.Command, m protoreflect.Message, fd protoreflect.FieldDescriptor, opt *forgepb.ConfigFieldOptions) error {
	raw, source, ok, err := resolveRaw(cmd, fd, opt)
	if err != nil {
		return err
	}
	if !ok {
		return nil // unset & not required: leave proto zero value
	}

	val, err := parseValue(fd, raw)
	if err != nil {
		return fmt.Errorf("invalid value %q for config field %s (from %s): %w", raw, opt.GetEnvVar(), source, err)
	}
	m.Set(fd, val)
	return nil
}

// resolveRaw applies the documented precedence and returns the raw string
// to parse. ok=false means "leave the field at its zero value" (an unset,
// non-required field with no default). A required field that resolves to
// nothing is an error. This mirrors the switch in the generated loadField.
func resolveRaw(cmd *cobra.Command, fd protoreflect.FieldDescriptor, opt *forgepb.ConfigFieldOptions) (raw, source string, ok bool, err error) {
	flagName := opt.GetFlag()
	envVar := opt.GetEnvVar()
	def := opt.GetDefaultValue()
	hasDefault := def != ""

	// Sensitive fields resolve from env / Secret mount ONLY: never from a
	// CLI flag (no flag is registered for them) and never from an inline
	// proto default (a literal secret default would be a leak). They fall
	// straight through to the env lookup below, then required-or-zero.
	if opt.GetSensitive() {
		flagName = ""
		hasDefault = false
	}

	// Flag wins when explicitly changed on THIS invocation.
	if cmd != nil && flagName != "" && cmd.Flags().Changed(flagName) {
		f := cmd.Flags().Lookup(flagName)
		if f != nil {
			return f.Value.String(), "flag --" + flagName, true, nil
		}
	}

	// Then env, honoring the empty-string rule for the field's kind.
	if envVar != "" {
		if v, present := os.LookupEnv(envVar); present && (allowEmptyEnv(fd) || v != "") {
			return v, "env " + envVar, true, nil
		}
	}

	// Then default.
	if hasDefault {
		return def, "default", true, nil
	}

	// Nothing set: error if required, otherwise leave the zero value.
	if opt.GetRequired() {
		if flagName != "" {
			return "", "", false, fmt.Errorf("required config field %s is not set (env: %s, flag: --%s)", fd.Name(), envVar, flagName)
		}
		return "", "", false, fmt.Errorf("required config field %s is not set (env: %s)", fd.Name(), envVar)
	}
	return "", "", false, nil
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
