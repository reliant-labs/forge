package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/spf13/cobra"

	"github.com/reliant-labs/forge/internal/cli/cmdutil"
	"github.com/reliant-labs/forge/internal/envutil"
	"github.com/reliant-labs/forge/internal/secrets"
)

// newSecretCmd is the `forge secret` group: the developer-facing way to
// put values into an env's FileSecrets store.
//
// The store is a single gitignored YAML file (env-var name -> value).
// These commands exist so adding a secret does not require knowing the
// file's shape, and so a value never lands in shell history — but the file
// is plain YAML, so hand-editing it is equally fine.
func newSecretCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "secret",
		Short: "Manage an environment's local secret store",
		Long: `Manage the gitignored YAML secret store a dev/e2e environment declares
via forge.FileSecrets — a flat map of env-var NAME to value.

A secret is declared ONCE in KCL as a reference (EnvVar.secret_ref); its
value lives here and never enters git or KCL render output. A value only
reaches a service that DECLARES it, so putting something here that no
service references does nothing — config belongs in deploy/kcl/<env>/config.k.

  forge secret ensure dev          # create the file + report missing values
  forge secret set    dev STRIPE_SECRET_KEY
  forge secret unset  dev STRIPE_SECRET_KEY
  forge secret list   dev          # names + presence, never values
  forge secret migrate dev         # convert a legacy .env file to YAML`,
	}
	cmd.AddCommand(
		newSecretSetCmd(),
		newSecretUnsetCmd(),
		newSecretListCmd(),
		newSecretEnsureCmd(),
		newSecretMigrateCmd(),
	)
	return cmdutil.StrictGroup(cmd)
}

func newSecretSetCmd() *cobra.Command {
	var fromFile string
	cmd := &cobra.Command{
		Use:   "set <environment> <KEY>",
		Short: "Set one secret value (read from stdin)",
		Args:  cobra.ExactArgs(2),
		Long: `Set a single secret in the environment's secret store.

The VALUE is read from stdin, never from argv — an argv value would land
in shell history and in the process table. Pipe it, or type it and press
Ctrl-D:

  printf '%s' "$TOKEN" | forge secret set dev STRIPE_SECRET_KEY
  forge secret set dev TLS_KEY --from-file ./key.pem

A trailing newline is trimmed. Multi-line values (a PEM key, a JSON blob)
are written as a YAML block scalar and round-trip unchanged.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runSecretSet(cmd.Context(), args[0], args[1], fromFile, cmd.InOrStdin(), cmd.OutOrStdout())
		},
	}
	cmd.Flags().StringVar(&fromFile, "from-file", "", "Read the value from a file instead of stdin")
	return cmd
}

func newSecretUnsetCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "unset <environment> <KEY>",
		Short: "Remove one secret from the store",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runSecretUnset(cmd.Context(), args[0], args[1], cmd.OutOrStdout())
		},
	}
}

func newSecretListCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "list <environment>",
		Short: "List declared secrets and whether each has a value",
		Args:  cobra.ExactArgs(1),
		Long: `List every secret the environment's KCL declares, and whether the store
holds a value for it. Values are NEVER printed.

Also reports keys in the store that no service declares — those are inert
(nothing injects them) and are usually either a typo or config that belongs
in deploy/kcl/<env>/config.k.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runSecretList(cmd.Context(), args[0], cmd.OutOrStdout())
		},
	}
}

func newSecretEnsureCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "ensure <environment>",
		Short: "Create the secret store and report missing values",
		Args:  cobra.ExactArgs(1),
		Long: `Create the environment's secret store (0600) if absent and list every
declared secret that has no value yet.

Exits non-zero when a declared secret is missing a value, so it works as a
setup gate in a task/Makefile before 'forge env up'.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runSecretEnsure(cmd.Context(), args[0], cmd.OutOrStdout())
		},
	}
}

func newSecretMigrateCmd() *cobra.Command {
	var dryRun bool
	cmd := &cobra.Command{
		Use:   "migrate <environment>",
		Short: "Convert a legacy .env secrets file into the YAML store",
		Args:  cobra.ExactArgs(1),
		Long: `Convert a legacy dotenv into the FileSecrets YAML store, then delete the
original. Run with --dry-run first to see exactly which keys move.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runSecretMigrate(cmd.Context(), args[0], dryRun, cmd.OutOrStdout())
		},
	}
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "Show what would move without writing anything")
	return cmd
}

// secretStorePath resolves the env's secret file from its KCL provider
// declaration. It requires a FileSecrets provider: these commands manage
// that store specifically, and pointing them elsewhere would silently
// write a file nothing reads.
func secretStorePath(ctx context.Context, envName string) (string, *KCLEntities, error) {
	projectDir := projectDirForKCL()
	entities, err := RenderKCL(ctx, projectDir, envName)
	if err != nil {
		return "", nil, fmt.Errorf("render KCL: %w", err)
	}
	sp := entities.SecretProvider
	if sp == nil {
		return "", nil, fmt.Errorf(
			"env %q declares no secret_provider\n"+
				"fix: add `secret_provider = forge.FileSecrets {path = \"secrets/%s.yaml\"}` to the Bundle in deploy/kcl/%s/main.k",
			envName, envName, envName)
	}
	if sp.Type != "file" {
		return "", nil, fmt.Errorf(
			"env %q declares a %q secret_provider, not FileSecrets\n"+
				"fix: forge secret migrate %s   (converts the file, then switch the KCL to forge.FileSecrets)",
			envName, sp.Type, envName)
	}
	path := sp.Path
	if path == "" {
		path = filepath.Join("secrets", envName+".yaml")
	}
	if !filepath.IsAbs(path) {
		path = filepath.Join(projectDir, path)
	}
	return path, entities, nil
}

// loadStore reads the store, treating a missing file as empty so `set` and
// `ensure` work on a fresh clone.
func loadStore(path string) (map[string]string, error) {
	values, err := secrets.ReadSecretFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return map[string]string{}, nil
		}
		return nil, err
	}
	return values, nil
}

func runSecretSet(ctx context.Context, envName, key, fromFile string, stdin io.Reader, out io.Writer) error {
	if !secrets.ValidSecretKey(key) {
		return fmt.Errorf("%q is not a valid env-var name (want [A-Za-z_][A-Za-z0-9_]*)", key)
	}
	path, _, err := secretStorePath(ctx, envName)
	if err != nil {
		return err
	}

	var raw []byte
	if fromFile != "" {
		raw, err = os.ReadFile(fromFile)
		if err != nil {
			return fmt.Errorf("read --from-file: %w", err)
		}
	} else {
		raw, err = io.ReadAll(stdin)
		if err != nil {
			return fmt.Errorf("read value from stdin: %w", err)
		}
	}
	value := strings.TrimRight(string(raw), "\r\n")
	if value == "" {
		return fmt.Errorf(
			"refusing to write an empty value for %s\n"+
				"fix: pipe the value in, e.g.  printf '%%s' \"$TOKEN\" | forge secret set %s %s",
			key, envName, key)
	}

	values, err := loadStore(path)
	if err != nil {
		return err
	}
	values[key] = value
	if err := secrets.WriteSecretFile(path, values); err != nil {
		return err
	}
	// Never echo the value — only that it landed, and how big it was.
	fmt.Fprintf(out, "set %s (%d bytes) in %s\n", key, len(value), path)
	return nil
}

func runSecretUnset(ctx context.Context, envName, key string, out io.Writer) error {
	path, _, err := secretStorePath(ctx, envName)
	if err != nil {
		return err
	}
	values, err := loadStore(path)
	if err != nil {
		return err
	}
	if _, ok := values[key]; !ok {
		return fmt.Errorf("%s is not set in %s", key, path)
	}
	delete(values, key)
	if err := secrets.WriteSecretFile(path, values); err != nil {
		return err
	}
	fmt.Fprintf(out, "unset %s in %s\n", key, path)
	return nil
}

func runSecretList(ctx context.Context, envName string, out io.Writer) error {
	path, entities, err := secretStorePath(ctx, envName)
	if err != nil {
		return err
	}
	present, err := loadStore(path)
	if err != nil {
		return err
	}
	declared := declaredSecretNames(entities)

	fmt.Fprintf(out, "secret store: %s\n\n", path)
	if len(declared) == 0 {
		fmt.Fprintln(out, "no secrets declared in KCL (nothing to resolve)")
	}
	for _, name := range declared {
		mark := "MISSING"
		if _, ok := present[name]; ok {
			mark = "set"
		}
		fmt.Fprintf(out, "  %-34s %s\n", name, mark)
	}

	// Keys nobody declares are inert under declaration-scoped injection.
	// Surfacing them is what keeps the store from silently accumulating
	// config that belongs in KCL.
	var orphans []string
	for k := range present {
		if !containsString(declared, k) {
			orphans = append(orphans, k)
		}
	}
	if len(orphans) > 0 {
		sort.Strings(orphans)
		fmt.Fprintf(out, "\n%d key(s) in the store that no service declares (inert — nothing injects them):\n", len(orphans))
		for _, o := range orphans {
			fmt.Fprintf(out, "  %s\n", o)
		}
		fmt.Fprintf(out, "fix: declare it with `forge.EnvVar {name = \"%s\", secret_ref = \"...\"}`,\n", orphans[0])
		fmt.Fprintln(out, "     move it to deploy/kcl/<env>/config.k if it is not a credential, or remove it.")
	}
	return nil
}

func runSecretEnsure(ctx context.Context, envName string, out io.Writer) error {
	path, entities, err := secretStorePath(ctx, envName)
	if err != nil {
		return err
	}
	values, err := loadStore(path)
	if err != nil {
		return err
	}
	if _, statErr := os.Stat(path); errors.Is(statErr, os.ErrNotExist) {
		if werr := secrets.WriteSecretFile(path, values); werr != nil {
			return werr
		}
	}
	fmt.Fprintf(out, "secret store ready: %s\n", path)

	var missing []string
	for _, name := range declaredSecretNames(entities) {
		if _, ok := values[name]; !ok {
			missing = append(missing, name)
		}
	}
	if len(missing) == 0 {
		fmt.Fprintln(out, "all declared secrets have values")
		return nil
	}
	fmt.Fprintf(out, "\n%d declared secret(s) have no value yet:\n", len(missing))
	for _, m := range missing {
		fmt.Fprintf(out, "  %s\n", m)
	}
	fmt.Fprintf(out, "\nfix: forge secret set %s <KEY>\n", envName)
	return fmt.Errorf("%d secret(s) missing a value", len(missing))
}

func runSecretMigrate(ctx context.Context, envName string, dryRun bool, out io.Writer) error {
	projectDir := projectDirForKCL()

	// Source: the legacy dotenv. Try the conventional spellings rather
	// than requiring the KCL still declare the removed provider — the
	// provider now hard-errors, so the KCL has usually been edited first.
	var src string
	for _, cand := range []string{
		".env." + envName + ".secrets",
		".env." + envName,
		".env",
	} {
		p := filepath.Join(projectDir, cand)
		if _, err := os.Stat(p); err == nil {
			src = p
			break
		}
	}
	if src == "" {
		return fmt.Errorf("no legacy .env file found for env %q (looked for .env.%s.secrets, .env.%s, .env)", envName, envName, envName)
	}

	values, err := envutil.ParseDotEnv(src)
	if err != nil {
		return fmt.Errorf("read %s: %w", src, err)
	}

	dstRel := filepath.Join("secrets", envName+".yaml")
	if sp := secretProviderPathForEnv(ctx, envName); sp != "" {
		dstRel = sp
	}
	dst := dstRel
	if !filepath.IsAbs(dst) {
		dst = filepath.Join(projectDir, dst)
	}

	keys := make([]string, 0, len(values))
	for k := range values {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	fmt.Fprintf(out, "%s  ->  %s\n\n", src, dst)
	var invalid []string
	for _, k := range keys {
		if !secrets.ValidSecretKey(k) {
			invalid = append(invalid, k)
			continue
		}
		fmt.Fprintf(out, "  %s\n", k)
	}
	if len(invalid) > 0 {
		return fmt.Errorf("cannot migrate: %d key(s) are not valid env-var names: %s",
			len(invalid), strings.Join(invalid, ", "))
	}
	if dryRun {
		fmt.Fprintf(out, "\ndry-run: nothing written (%d key(s) would move)\n", len(keys))
		return nil
	}

	// Merge onto whatever the store already holds so a re-run is additive.
	existing, err := loadStore(dst)
	if err != nil {
		return err
	}
	for k, v := range values {
		existing[k] = v
	}
	if err := secrets.WriteSecretFile(dst, existing); err != nil {
		return err
	}
	if err := os.Remove(src); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("remove %s: %w", src, err)
	}

	fmt.Fprintf(out, "\nmoved %d secret(s); removed %s\n", len(keys), src)
	fmt.Fprintf(out, "\nnow declare the provider in deploy/kcl/%s/main.k:\n", envName)
	fmt.Fprintf(out, "    secret_provider = forge.FileSecrets {path = %q}\n", dstRel)
	return nil
}

// secretProviderPathForEnv returns the env's declared store path, or ""
// when the KCL cannot be rendered (e.g. it still declares the removed
// dotenv provider, which is exactly when migrate is run).
func secretProviderPathForEnv(ctx context.Context, envName string) string {
	entities, err := RenderKCL(ctx, projectDirForKCL(), envName)
	if err != nil || entities == nil || entities.SecretProvider == nil {
		return ""
	}
	if entities.SecretProvider.Type != "file" {
		return ""
	}
	return entities.SecretProvider.Path
}

// declaredSecretNames is the sorted, de-duplicated set of env-var names
// every service in the env declares via secret_ref.
func declaredSecretNames(e *KCLEntities) []string {
	seen := map[string]struct{}{}
	for _, r := range secretRefsFromEntities(e) {
		if r.EnvName != "" {
			seen[r.EnvName] = struct{}{}
		}
	}
	out := make([]string, 0, len(seen))
	for k := range seen {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func containsString(haystack []string, needle string) bool {
	for _, h := range haystack {
		if h == needle {
			return true
		}
	}
	return false
}
