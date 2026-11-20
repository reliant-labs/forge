package cli

import (
	"fmt"
	"io"
	"strings"
	"sync"

	"github.com/reliant-labs/forge/pkg/release"
	"github.com/reliant-labs/forge/pkg/telemetry"
)

// telemetryUncollectedNoted dedupes the warning per env and process: a
// deploy that writes an env's bundle more than once says it once.
var telemetryUncollectedNoted sync.Map

// checkBundleTelemetry is the hosted-telemetry step of a bundle write to
// reader's control plane. One that does not decode release.BundleFeatureTelemetry
// gets the shape without the env's hosted targets (BundleReader.LimitShape), so
// the person deploying is WARNED, naming each target they declared that will go
// uncollected: a warning and not a refusal, because the env still deploys and
// runs. A target forge rendered on its own (telemetry.OriginDefault, a
// ManagedDatabase's default) is withheld silently: nobody asked for it, so it
// is no news to them. One that
// decodes it and advertises caps of its own (GetCapabilities.telemetry_limits)
// holds the targets to them, which the shape build could not know: a target
// over them is refused here, located, rather than dropped by the platform.
func checkBundleTelemetry(w io.Writer, env string, shape release.Shape, reader *release.BundleReader) error {
	if len(shape.Telemetry) == 0 {
		return nil
	}
	if msg := reader.TelemetryWarning(env, shape); msg != "" {
		if _, seen := telemetryUncollectedNoted.LoadOrStore(env, true); !seen {
			fmt.Fprintf(w, "[bundle] Warning: %s\n", strings.ReplaceAll(msg, "\n", "\n[bundle]   "))
		}
		return nil
	}
	if reader == nil || reader.Telemetry == nil {
		return nil // the shape was built under the defaults
	}
	targets := make([]telemetry.Target, len(shape.Telemetry))
	for i, t := range shape.Telemetry {
		targets[i] = t.Target
	}
	if err := checkHostedTelemetry(env, nil, targets, reader.TelemetryCaps()); err != nil {
		return fmt.Errorf("the control plane holds env %s's hosted telemetry to caps it advertises (%d series per scrape, %d pulled targets): %w",
			env, reader.TelemetryCaps().MaxSampleLimit, reader.TelemetryCaps().MaxPulledTargets, err)
	}
	return nil
}
