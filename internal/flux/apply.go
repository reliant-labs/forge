package flux

// Writing the pointer, and nothing else.
//
// THIS IS THE ONLY CLUSTER WRITE ON THE RECONCILED PATH. forge applies two
// small objects per cluster and stops; Flux applies the env. That boundary is
// the design rather than an implementation detail, so it is worth stating what
// is NOT here: no namespace of the env's own, no Secret projection, no CRD
// pass, no rollout wait over the env's Deployments. Every one of those is
// something forge does on the DIRECT path and must not do here, because doing
// it would make two things authorities over the same objects — and the one
// that lost would keep reverting the other.
//
// SERVER-SIDE APPLY, with forge's own field manager. The pointer is updated in
// place on every deploy (same names, new digest), so the write has to be an
// apply rather than a create-or-replace: a replace would drop the status
// subresource's owner and a create would fail on the second deploy. SSA also
// makes the write IDEMPOTENT, which matters because a deploy that is retried
// after a lost response must not look different from one that was not.
//
// --force-conflicts is deliberate and matches forge's main apply path. The
// pointer is forge's to own: a field previously set by a human's `kubectl
// apply` (manager `kubectl-client-side-apply`) would otherwise abort the whole
// write with a conflict, which turns "someone debugged this once" into a
// permanently un-deployable env.

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/reliant-labs/forge/internal/cluster"
)

// Apply writes one cluster's pointer, server-side, as [FieldManager].
//
// The namespace is ensured first. Flux's own chart creates `flux-system`, so
// in the ordinary case this is a no-op — but a cluster where the chart was
// installed into a different namespace, or where someone deleted it, would
// otherwise fail the write with `namespaces "flux-system" not found`, which
// names the symptom and not the cause.
func Apply(ctx context.Context, p Pointer) error {
	if p.Source.Cluster == "" {
		return fmt.Errorf("flux: pointer has no cluster to apply into")
	}
	kctx := p.Source.Cluster
	if err := cluster.EnsureNamespace(ctx, kctx, Namespace); err != nil {
		return fmt.Errorf("flux: ensure %s in %s: %w", Namespace, kctx, err)
	}
	stream, err := Encode(p.All())
	if err != nil {
		return err
	}
	if err := applyServerSide(ctx, kctx, stream); err != nil {
		return fmt.Errorf("flux: write %s's desired-state pointer to %s: %w", p.Source.Name, kctx, err)
	}
	return nil
}

// Encode marshals objects into one `---`-joined YAML stream, in order.
//
// Exported because `--dry-run` and the deploy report show the pointer rather
// than describing it: an operator asking "what is forge about to write" should
// read the bytes, and a second renderer for display could disagree with the
// one that writes.
func Encode(objs []Object) (string, error) {
	var b strings.Builder
	for i, o := range objs {
		var buf bytes.Buffer
		enc := yaml.NewEncoder(&buf)
		enc.SetIndent(2)
		if err := enc.Encode(o.Doc); err != nil {
			return "", fmt.Errorf("flux: encode %s/%s: %w", o.Kind, o.Name, err)
		}
		if err := enc.Close(); err != nil {
			return "", fmt.Errorf("flux: encode %s/%s: %w", o.Kind, o.Name, err)
		}
		if i > 0 {
			b.WriteString("---\n")
		}
		b.Write(buf.Bytes())
	}
	return b.String(), nil
}

// applyServerSide pipes the stream into kubectl.
//
// A seam (var, below) rather than a direct call, because every test of the
// pointer path would otherwise need a cluster: what those tests state is WHICH
// BYTES reach which context, and that is observable exactly here.
var applyServerSide = func(ctx context.Context, kctx, stream string) error {
	cmd := exec.CommandContext(ctx, "kubectl", cluster.KubectlArgs(kctx,
		"apply", "--server-side", "--force-conflicts",
		"--field-manager="+FieldManager, "-f", "-")...)
	cmd.Stdin = strings.NewReader(stream)
	var errBuf bytes.Buffer
	cmd.Stdout = os.Stdout
	cmd.Stderr = &errBuf
	if err := cmd.Run(); err != nil {
		if msg := strings.TrimSpace(errBuf.String()); msg != "" {
			fmt.Fprintln(os.Stderr, msg)
			return fmt.Errorf("%w: %s", err, firstLine(msg))
		}
		return err
	}
	return nil
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}
