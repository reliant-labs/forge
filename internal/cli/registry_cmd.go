package cli

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"

	"github.com/spf13/cobra"

	"github.com/reliant-labs/forge/internal/buildtarget"
	"github.com/reliant-labs/forge/internal/cli/cmdutil"
	"github.com/reliant-labs/forge/internal/cliutil"
)

// `forge registry` is what a CI job needs to work from the env's registry
// DECLARATION alone: log docker in to the registry deploy/kcl/<env>/main.k
// declares, and read back the digest-pinned ref of what `forge build <env>
// --push` pushed there. Neither command takes a registry or a host — there is
// no flag or argument that could carry one. The only inputs are the env name
// and, for login, a credential, which is not a registry pointer.

func newRegistryCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "registry",
		Short: "Log in to and read refs from the image registry an env's KCL declares",
		Long: `Work with the image registry an environment DECLARES — forge.ClusterTarget.registry,
or forge.ControlPlane.registry for a hosted env, in deploy/kcl/<env>/main.k.

The registry is never passed to forge: these commands read it from the env's
KCL, exactly as ` + "`forge build <env> --push`" + ` and ` + "`forge env deploy <env>`" + ` do.`,
	}
	cmd.AddCommand(newRegistryLoginCmd(), newRegistryRefCmd())
	return cmdutil.StrictGroup(cmd)
}

// dockerLogin runs `docker login <host> -u <username> --password-stdin` with
// password on stdin. A var so tests observe the login instead of running it.
var dockerLogin = func(ctx context.Context, host, username string, password io.Reader) error {
	c := exec.CommandContext(ctx, "docker", "login", host, "--username", username, "--password-stdin")
	c.Stdin = password
	c.Stdout = os.Stdout
	c.Stderr = os.Stderr
	if err := c.Run(); err != nil {
		return fmt.Errorf("docker login %s: %w", host, err)
	}
	return nil
}

func newRegistryLoginCmd() *cobra.Command {
	var (
		username      string
		passwordStdin bool
	)
	cmd := &cobra.Command{
		Use:   "login <environment> --username <user> --password-stdin",
		Short: "docker login to the registry the env's KCL declares",
		Long: `Log docker in to the image registry deploy/kcl/<env>/main.k declares, so that
` + "`forge build <env> --push`" + ` and any signing / SBOM / scanning step that follows
can reach it.

The registry HOST comes from the env's KCL and nowhere else. The credential
comes from --username and stdin (--password-stdin), the same contract as
` + "`docker login`" + `:

  echo "$GITHUB_TOKEN" | forge registry login prod --username "$GITHUB_ACTOR" --password-stdin

A k3d-local registry (localhost / *.localhost) takes no credentials, so
logging in to one is a no-op.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			env := args[0]
			if err := validateBuildEnvArg(env); err != nil {
				return err
			}
			if !passwordStdin {
				return cliutil.UserErr("forge registry login", "the credential is read from stdin, and --password-stdin was not given", "",
					fmt.Sprintf("pipe it in: echo \"$TOKEN\" | forge registry login %s --username <user> --password-stdin", env))
			}
			if username == "" {
				return cliutil.UserErr("forge registry login", "--username is required", "", "pass the registry user the credential belongs to")
			}
			registry, err := declaredRegistryOf(cmd.Context(), "forge registry login "+env, env)
			if err != nil {
				return err
			}
			host := registryHost(registry)
			if isLocalRegistryHost(host) {
				fmt.Printf("[registry] %s is a local registry (declared in deploy/kcl/%s/main.k): no login needed\n", registry, env)
				return nil
			}
			fmt.Printf("[registry] logging in to %s (declared in deploy/kcl/%s/main.k: %s)\n", host, env, registry)
			return dockerLogin(cmd.Context(), host, username, cmd.InOrStdin())
		},
	}
	cmd.Flags().StringVarP(&username, "username", "u", "", "Registry user the credential belongs to (e.g. $GITHUB_ACTOR, _json_key, oauth2accesstoken, AWS)")
	cmd.Flags().BoolVar(&passwordStdin, "password-stdin", false, "Read the credential from stdin (required; a credential never belongs on the command line)")
	return cmd
}

func newRegistryRefCmd() *cobra.Command {
	var (
		image        string
		githubOutput bool
	)
	cmd := &cobra.Command{
		Use:   "ref <environment> [--image <name>]",
		Short: "Print the digest-pinned ref `forge build <env> --push` pushed",
		Long: `Print <registry>/<image>@<digest> for the image the last ` + "`forge build <env> --push`" + `
pushed — the immutable reference a signing, SBOM, provenance or vulnerability
scan step should act on. It is read from .forge/state/ (what the build
recorded), so the registry is the one the env's KCL declares.

Without --image it is the project image; --image names another (a frontend,
a DockerBuild workload).

--github-output also appends ref=, image= and digest= to the file GitHub
Actions names in $GITHUB_OUTPUT, so later steps read them as step outputs.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			env := args[0]
			if err := validateBuildEnvArg(env); err != nil {
				return err
			}
			ref, err := pushedImageRef(projectDirForKCL(), env, image)
			if err != nil {
				return err
			}
			fmt.Println(ref.String())
			if githubOutput {
				return ref.appendGitHubOutput()
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&image, "image", "", "The image to print (default: the project image)")
	cmd.Flags().BoolVar(&githubOutput, "github-output", false, "Also append ref=, image= and digest= to $GITHUB_OUTPUT")
	return cmd
}

// declaredRegistryOf renders env and returns the registry its KCL declares,
// or the runbook naming the file and field to set. context is the command.
func declaredRegistryOf(ctx context.Context, context, env string) (string, error) {
	ents, err := renderBuildKCL(ctx, projectDirForKCL(), env)
	if err != nil {
		return "", err
	}
	if r := declaredRegistry(ents); r != "" {
		return r, nil
	}
	return "", undeclaredRegistryError(context, env, ents)
}

// registryHost is the host docker logs in to for a registry reference: the
// part before the first `/` when it looks like a host (has a `.` or `:`, or is
// localhost), else Docker Hub — docker's own rule for a bare `org/img`.
func registryHost(registry string) string {
	first, _, _ := strings.Cut(registry, "/")
	if strings.ContainsAny(first, ".:") || first == "localhost" {
		return first
	}
	return "docker.io"
}

// isLocalRegistryHost mirrors kcl/base.k is_local_registry: a host-local
// registry (localhost, 127.0.0.1, *.localhost) takes no credentials.
func isLocalRegistryHost(host string) bool {
	name, _, _ := strings.Cut(host, ":")
	return name == "localhost" || name == "127.0.0.1" || strings.HasSuffix(name, ".localhost")
}

// imageRef is a pushed image: <repository>@<digest>.
type imageRef struct {
	repository string // <registry>/<image>
	digest     string
}

func (r imageRef) String() string { return r.repository + "@" + r.digest }

func (r imageRef) appendGitHubOutput() error {
	path := os.Getenv("GITHUB_OUTPUT")
	if path == "" {
		return cliutil.UserErr("forge registry ref --github-output", "$GITHUB_OUTPUT is not set", "",
			"run it inside a GitHub Actions step, or drop --github-output and read the ref from stdout")
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return fmt.Errorf("open $GITHUB_OUTPUT: %w", err)
	}
	if _, err := fmt.Fprintf(f, "ref=%s\nimage=%s\ndigest=%s\n", r, r.repository, r.digest); err != nil {
		_ = f.Close()
		return fmt.Errorf("write $GITHUB_OUTPUT: %w", err)
	}
	// A failed close can drop the appended outputs; later steps would then
	// read an empty ref and sign nothing.
	if err := f.Close(); err != nil {
		return fmt.Errorf("close $GITHUB_OUTPUT: %w", err)
	}
	return nil
}

// pushedImageRef reads what `forge build <env> --push` recorded for image
// ("" = the project image) and returns its digest-pinned ref.
func pushedImageRef(projectDir, env, image string) (imageRef, error) {
	notPushed := func(what string) error {
		return cliutil.UserErr("forge registry ref "+env,
			fmt.Sprintf("%s has no pushed digest recorded for env %q", what, env), "",
			fmt.Sprintf("run forge build %s --push first — it records the digest of what it pushed", env))
	}
	if image == "" {
		st, err := ReadBuildState(projectDir, env)
		if err != nil {
			return imageRef{}, err
		}
		if st == nil || st.Digest == "" || st.Registry == "" {
			return imageRef{}, notPushed("the project image")
		}
		return imageRef{repository: strings.TrimSuffix(st.Registry, "/") + "/" + st.Image, digest: st.Digest}, nil
	}
	st, err := buildtarget.ReadState(projectDir, env, image)
	if err != nil {
		return imageRef{}, err
	}
	if st == nil || st.Digest == "" || st.Registry == "" {
		return imageRef{}, notPushed(fmt.Sprintf("image %q", image))
	}
	return imageRef{repository: strings.TrimSuffix(st.Registry, "/") + "/" + st.Image, digest: st.Digest}, nil
}
