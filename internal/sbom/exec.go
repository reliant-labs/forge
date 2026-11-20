package sbom

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
)

// execRunner runs the real syft binary.
type execRunner struct{}

// Run execs bin with args. syft's own update check is switched off: a cut
// must not phone a third party, and the version forge runs is pinned anyway.
// stderr is kept for the error only; with -q a successful run writes none.
func (execRunner) Run(ctx context.Context, bin string, args []string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, bin, args...) //nolint:gosec // bin is forge's pinned syft; args are built by syftArgs
	cmd.Env = append(os.Environ(), "SYFT_CHECK_FOR_APP_UPDATE=false")
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		if msg := strings.TrimSpace(stderr.String()); msg != "" {
			return nil, fmt.Errorf("%w: %s", err, msg)
		}
		return nil, err
	}
	return stdout.Bytes(), nil
}
