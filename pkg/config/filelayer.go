package config

// First-class config-FILE loading layer.
//
// This file adds a config file as a real, always-wired precedence layer to the
// descriptor-driven loader. The full precedence is:
//
//	defaults  →  config file  →  environment  →  flags
//	(earliest)                                   (wins)
//
// Each later layer OVERRIDES the earlier ones. The file layer is NOT optional
// and NOT dormant: RegisterFlags always registers a persistent `--config`
// flag, and Load always consults it (plus the documented env override).
//
// Resolution of the file path (see resolveConfigPath):
//
//   - the `--config <path>` flag, when explicitly set on this invocation, OR
//   - the documented env override FORGE_CONFIG (env var name = ConfigPathEnv).
//
// The flag wins over the env override. If neither is given, NO file is loaded
// and the loader proceeds with defaults → env → flags. That is normal
// precedence, not a fallback.
//
// LOUD-ON-MISSING (no silent fallback): when a config path IS explicitly given
// (flag or env) but the file is missing, unreadable, or invalid, Load returns
// an error and aborts. A bad explicit path is never swallowed.
//
// Formats: protojson for `.json`; for `.yaml`/`.yml` the bytes are converted
// YAML→JSON (sigs.k8s.io/yaml) and then unmarshaled with protojson. Unmarshal
// is proto-native and generic over ANY config message: it honors the proto's
// own JSON field names and descends into nested config blocks automatically.
//
// Sensitive fields: the file MAY set a sensitive field (it is a file on disk,
// not shell history), exactly as env may. Sensitive fields still never get a
// flag. The protojson unmarshal does not know about the (sensitive) annotation
// — it simply sets whatever the file names — which is the intended behavior:
// file/env yes, flag no.

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	sigsyaml "sigs.k8s.io/yaml"
)

// ConfigFlag is the name of the always-registered persistent flag that points
// the loader at a config file. RegisterFlags registers it unconditionally.
const ConfigFlag = "config"

// ConfigPathEnv is the documented environment variable that supplies the
// config-file path when the --config flag is not set. The flag, when set,
// takes precedence over this env var. Set FORGE_CONFIG to load a config file
// without passing --config (e.g. in a container entrypoint).
const ConfigPathEnv = "FORGE_CONFIG"

// resolveConfigPath returns the explicit config-file path for this invocation
// and whether one was given. The --config flag (when Changed) wins over the
// FORGE_CONFIG env var. An empty FORGE_CONFIG is treated as unset. When this
// returns ok=false, no file is loaded (normal defaults→env→flags precedence).
func resolveConfigPath(cmd *cobra.Command) (path string, ok bool) {
	if cmd != nil && cmd.Flags().Changed(ConfigFlag) {
		if f := cmd.Flags().Lookup(ConfigFlag); f != nil {
			if v := f.Value.String(); v != "" {
				return v, true
			}
		}
	}
	if v, present := os.LookupEnv(ConfigPathEnv); present && v != "" {
		return v, true
	}
	return "", false
}

// applyConfigFile reads the file at path and OVERLAYS it onto msg, preserving
// the defaults layer for fields the file omits. It is the file precedence
// layer (between defaults and env).
//
// Implementation note: protojson.Unmarshal RESETS its target before decoding,
// so it cannot decode directly into the defaults-populated msg without wiping
// the defaults. Instead the file is decoded into a FRESH message of the same
// type and then proto.Merge'd onto msg: Merge copies the file's populated
// fields over the defaults and recurses into nested blocks, while fields the
// file did not populate keep their default. (Proto3 scalar presence is
// value-based: a file that explicitly writes a scalar's zero value cannot be
// distinguished from omitting it — declare such a field with a non-zero
// default or as optional if that distinction matters.)
//
// Any failure — unreadable file, unknown extension, malformed JSON/YAML, or a
// document that does not match the config schema — is returned as a loud error.
// Callers only invoke this when a path was EXPLICITLY given, so an error here
// always aborts Load (no silent fallback).
func applyConfigFile(msg proto.Message, path string) error {
	raw, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("config file %q does not exist (set --%s or %s to a readable file, or omit both)", path, ConfigFlag, ConfigPathEnv)
		}
		return fmt.Errorf("config file %q: %w", path, err)
	}

	jsonBytes, err := toJSON(path, raw)
	if err != nil {
		return fmt.Errorf("config file %q: %w", path, err)
	}

	// Decode into a fresh message of the same type (protojson resets it),
	// then merge the populated fields onto msg. DiscardUnknown=false makes a
	// stray/typo'd key a loud error rather than a silent drop.
	fileMsg := msg.ProtoReflect().New().Interface()
	opts := protojson.UnmarshalOptions{DiscardUnknown: false}
	if err := opts.Unmarshal(jsonBytes, fileMsg); err != nil {
		return fmt.Errorf("config file %q: invalid config document: %w", path, err)
	}
	proto.Merge(msg, fileMsg)
	return nil
}

// toJSON returns protojson-ready JSON bytes for a config file, dispatching on
// the file extension: .json passes through; .yaml/.yml are converted via
// sigs.k8s.io/yaml (which round-trips through the YAML and JSON codecs). An
// unrecognized extension is a loud error — the format must be explicit.
func toJSON(path string, raw []byte) ([]byte, error) {
	switch ext := strings.ToLower(filepath.Ext(path)); ext {
	case ".json":
		return raw, nil
	case ".yaml", ".yml":
		j, err := sigsyaml.YAMLToJSON(raw)
		if err != nil {
			return nil, fmt.Errorf("converting YAML to JSON: %w", err)
		}
		return j, nil
	default:
		return nil, fmt.Errorf("unsupported config file extension %q (want .json, .yaml, or .yml)", ext)
	}
}
