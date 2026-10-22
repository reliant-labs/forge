package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/spf13/cobra"

	"github.com/reliant-labs/forge/pkg/release"
)

// `forge secret import` copies values that already exist into a store, once.
//
// It is step 1 of the per-Secret migration (design §7.1): the platform's
// Secrets were seeded by a shell script and live only in the cluster, and the
// store has to hold the same bytes before ESO may take over. Two properties
// make that safe to run, re-run and abandon halfway:
//
//   - CREATE-ONLY. Every write is check-and-set 0 (hosted, OpenBao) or "no
//     usable version yet" (Secret Manager). A name the store already holds is
//     reported and left alone, so a re-run can never overwrite a value a
//     person has since rotated. Rotation is `forge secret set`.
//   - IN MEMORY. A value read from the cluster goes straight into the write
//     and is never printed, logged or put in an error. Values reach the
//     command from a cluster Secret, stdin or --from-file; none is an
//     argument.

type importOptions struct {
	storeOptions
	// From is "<namespace>/<secret>": the cluster Secret to read.
	From string
	// Map is "key=NAME": which store name each Secret key fills.
	Map []string
	// ToBootstrap writes to the Secret Manager container of a BootstrapSecret
	// (Bundle.bootstrap_source) instead of the store.
	ToBootstrap bool
	DryRun      bool
	FromFile    string
}

func newSecretImportCmd() *cobra.Command {
	var (
		env  string
		opts importOptions
	)
	cmd := &cobra.Command{
		Use:   "import --env <environment> (--from <namespace>/<secret> --map key=NAME[,key=NAME...] | <NAME>)",
		Short: "Copy existing values into the env's secret store, create-only",
		Args:  cobra.MaximumNArgs(1),
		Long: `Copy values that already exist into a store-backed ExternalSecret's store.
This is the first step of migrating a Secret onto ESO: the store must hold the
bytes the cluster holds today before ESO takes the Secret over.

Create-only. A name the store already holds is reported and left untouched, so
re-running an import is always safe and can never overwrite a value that was
rotated since. Rotating is 'forge secret set'.

From a cluster Secret, using your own kube context:

  forge secret import --env prod --from control-plane-prod/control-plane-secrets \
      --map github_app_private_key=GITHUB_APP_PRIVATE_KEY

From stdin or a file, one name:

  forge secret import --env prod GITHUB_WEBHOOK_SECRET --from-file ./secret.txt

--to-bootstrap writes a BootstrapSecret's Google Secret Manager container (the
OpenBao seal key and Postgres password, which cannot live in the store they
bootstrap) in the project of Bundle.bootstrap_source:

  forge secret import --env prod --to-bootstrap \
      --from control-plane-prod/control-plane-openbao-unseal --map static-key=openbao-unseal-prod

The destination store comes from the ExternalSecret that reads each name
(--store picks one when two do). Google Secret Manager uses your Application
Default Credentials. No value is ever printed, and none is taken as an
argument. --dry-run reads the source and reports what would be created.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			envName, err := requireSecretEnv(env, "import --env <env> ...")
			if err != nil {
				return err
			}
			var name string
			if len(args) == 1 {
				name = args[0]
			}
			return runSecretImport(cmd.Context(), envName, name, opts, cmd.InOrStdin(), cmd.OutOrStdout())
		},
	}
	addSecretEnvFlag(cmd, &env)
	cmd.Flags().StringVar(&opts.From, "from", "", "Cluster Secret to read, as <namespace>/<secret> (uses your kube context)")
	cmd.Flags().StringSliceVar(&opts.Map, "map", nil, "key=NAME: the Secret key to read and the store name it fills (repeatable, or comma-separated)")
	cmd.Flags().StringVar(&opts.FromFile, "from-file", "", "Read the single NAME's value from a file instead of stdin")
	cmd.Flags().BoolVar(&opts.ToBootstrap, "to-bootstrap", false, "Write a BootstrapSecret's Google Secret Manager container (Bundle.bootstrap_source)")
	cmd.Flags().BoolVar(&opts.DryRun, "dry-run", false, "Read the source and report what would be created; write nothing")
	addStoreFlags(cmd, &opts.storeOptions)
	return cmd
}

// addStoreFlags registers the flags the store-source commands share.
func addStoreFlags(cmd *cobra.Command, o *storeOptions) {
	cmd.Flags().StringVar(&o.Store, "store", "", "Store kind to act on when two stores declare a name (hosted, openbao, gcpsm)")
	cmd.Flags().StringVar(&o.KubeContext, "kube-context", "", "Kube context to use (default: the env's declared cluster)")
}

// importItem is one value to write: its destination name and where the bytes
// come from. The value is held only between read and write.
type importItem struct {
	name  string
	value string
}

func runSecretImport(ctx context.Context, envName, name string, opts importOptions, stdin io.Reader, out io.Writer) error {
	if err := opts.validate(); err != nil {
		return err
	}
	e, err := renderEntitiesForSecrets(ctx, envName)
	if err != nil {
		return fmt.Errorf("render KCL: %w", err)
	}
	items, err := importSource(ctx, e, opts, name, stdin)
	if err != nil {
		return err
	}

	// Resolve every destination BEFORE writing anything, so a typo in the
	// third name does not leave the first two half-imported.
	type dest struct {
		item importItem
		ref  storeRef
	}
	dests := make([]dest, 0, len(items))
	for _, it := range items {
		ref, err := importDestination(e, envName, it.name, opts)
		if err != nil {
			return err
		}
		if err := validStoreName(ref, it.name); err != nil {
			return err
		}
		dests = append(dests, dest{it, ref})
	}

	open := map[storeRef]storeSource{}
	var failed []string
	for _, d := range dests {
		if opts.DryRun {
			fmt.Fprintf(out, "%s: would create in the %s store (%d bytes)\n", d.item.name, d.ref, len(d.item.value))
			continue
		}
		src, ok := open[d.ref]
		if !ok {
			src, err = openStoreSource(ctx, envName, e, d.ref, opts.storeOptions, true)
			if err != nil {
				return err
			}
			open[d.ref] = src
		}
		version, err := src.put(ctx, d.item.name, d.item.value, true)
		switch {
		case errors.Is(err, errStoreValueExists):
			fmt.Fprintf(out, "%s: exists in %s, left unchanged (compare it with: forge env secrets verify %s)\n", d.item.name, src.describe(), envName)
		case err != nil:
			failed = append(failed, d.item.name)
			fmt.Fprintf(out, "%s: FAILED: %v\n", d.item.name, redactValue(err, d.item.value))
		default:
			fmt.Fprintf(out, "%s: created v%d (%d bytes) in %s\n", d.item.name, version, len(d.item.value), src.describe())
		}
	}
	if len(failed) > 0 {
		return fmt.Errorf("%d name(s) were not imported: %s", len(failed), strings.Join(failed, ", "))
	}
	return nil
}

// importSource reads the values: from a cluster Secret (--from/--map), or one
// value from stdin or --from-file.
func importSource(ctx context.Context, e *KCLEntities, opts importOptions, name string, stdin io.Reader) ([]importItem, error) {
	if opts.From == "" {
		if len(opts.Map) > 0 {
			return nil, errors.New("--map only applies with --from")
		}
		if name == "" {
			return nil, errors.New("nothing to import: give --from <namespace>/<secret> with --map, or a NAME with a value on stdin or --from-file")
		}
		value, err := readSecretValue(opts.FromFile, stdin)
		if err != nil {
			return nil, err
		}
		if value == "" {
			return nil, fmt.Errorf("refusing to import an empty value for %s", name)
		}
		return []importItem{{name, value}}, nil
	}
	if name != "" || opts.FromFile != "" {
		return nil, errors.New("--from reads the cluster Secret; a NAME argument and --from-file are for the stdin form")
	}
	ns, secret, ok := strings.Cut(opts.From, "/")
	if !ok || ns == "" || secret == "" || strings.Contains(secret, "/") {
		return nil, fmt.Errorf("--from %q is not <namespace>/<secret>", opts.From)
	}
	pairs, err := parseKeyMap(opts.Map)
	if err != nil {
		return nil, err
	}
	kube, err := newKubeAccess(kubeContextFor(e, opts.storeOptions))
	if err != nil {
		return nil, err
	}
	sec, err := kube.GetSecret(ctx, ns, secret)
	if err != nil {
		return nil, err
	}
	var missing, notText []string
	items := make([]importItem, 0, len(pairs))
	for _, p := range pairs {
		raw, ok := sec.Data[p.key]
		switch {
		case !ok:
			missing = append(missing, p.key)
		case len(raw) == 0:
			missing = append(missing, p.key+" (empty)")
		case !utf8.Valid(raw):
			notText = append(notText, p.key)
		default:
			// Exactly the stored bytes: no trimming, so verify can compare.
			items = append(items, importItem{p.name, string(raw)})
		}
	}
	if len(missing) > 0 {
		return nil, fmt.Errorf("%s/%s has no value for key(s): %s", ns, secret, strings.Join(missing, ", "))
	}
	if len(notText) > 0 {
		return nil, fmt.Errorf("%s/%s key(s) %s are not text: a store value is a string, so binary data cannot be imported",
			ns, secret, strings.Join(notText, ", "))
	}
	return items, nil
}

type keyMapping struct{ key, name string }

// parseKeyMap parses --map key=NAME entries. A key may repeat only if it maps
// to different names; a name may not be filled twice.
func parseKeyMap(entries []string) ([]keyMapping, error) {
	if len(entries) == 0 {
		return nil, errors.New("--from needs --map key=NAME: which Secret keys to import, and the store name each fills")
	}
	var out []keyMapping
	seen := map[string]bool{}
	for _, entry := range entries {
		key, name, ok := strings.Cut(entry, "=")
		key, name = strings.TrimSpace(key), strings.TrimSpace(name)
		if !ok || key == "" || name == "" {
			return nil, fmt.Errorf("--map %q is not key=NAME", entry)
		}
		if seen[name] {
			return nil, fmt.Errorf("--map fills %s twice", name)
		}
		seen[name] = true
		out = append(out, keyMapping{key, name})
	}
	return out, nil
}

// importDestination decides which store a name is written to.
func importDestination(e *KCLEntities, envName, name string, opts importOptions) (storeRef, error) {
	if opts.ToBootstrap {
		bs := e.BootstrapSource
		if bs == nil || bs.Type != "gcpsm" || bs.Project == "" {
			return storeRef{}, fmt.Errorf("--to-bootstrap needs Bundle.bootstrap_source = forge.GCPSecretManager {project = ...} in env %q", envName)
		}
		var names []string
		if e.OpenBao != nil {
			names = e.OpenBao.bootstrapNames()
		}
		for _, n := range names {
			if n == name {
				return storeRef{Kind: release.StoreGCPSecretManager, Project: bs.Project}, nil
			}
		}
		return storeRef{}, fmt.Errorf("%s is not a BootstrapSecret of env %q (declared: %s)", name, envName, orNone(names))
	}
	decls := storeDeclarations(e)
	ref, found, err := selectStore(decls, name, opts.storeOptions)
	if err != nil {
		return storeRef{}, err
	}
	if !found {
		var declared []string
		for _, d := range decls {
			declared = append(declared, d.Name)
		}
		return storeRef{}, fmt.Errorf("%s is not read or generated by any store-backed ExternalSecret in env %q (declared: %s)\n"+
			"fix: declare it with a forge.StoreSource, or check the name", name, envName, orNone(dedupeSorted(declared)))
	}
	return ref, nil
}

func orNone(names []string) string {
	if len(names) == 0 {
		return "none"
	}
	sorted := append([]string(nil), names...)
	sort.Strings(sorted)
	return strings.Join(sorted, ", ")
}

// kubeContextFor is the context a command reaches the cluster through:
// --kube-context, else the env's declared cluster target. Never the kubeconfig's
// current context.
func kubeContextFor(e *KCLEntities, o storeOptions) string {
	if o.KubeContext != "" {
		return o.KubeContext
	}
	if e != nil && e.ClusterTarget != nil {
		return e.ClusterTarget.Cluster
	}
	return ""
}
