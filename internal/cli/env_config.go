// File: internal/cli/env_config.go
//
// `forge env config` — print the resolved per-workload configuration an
// environment's KCL declares.
//
// The question this answers is "what values is this environment actually
// going to hand my workloads", and it comes up constantly: which database am
// I connected to, which broker, which bucket, which upstream API. All of it
// is already computed — `forge env up` renders exactly these values and
// passes them to each process — but reading it back meant opening
// deploy/kcl/<env>/ and evaluating a template string in your head, then
// cross-referencing .forge/ports-<env>.json for any port that was resolved
// at launch.
//
// That gap has a bad failure mode on a shared machine. Someone who wants the
// database they are working on runs `docker ps`, picks the plausible-looking
// container, and gets SOMEONE's postgres — just not necessarily this
// project's, and the mistake reads as correct right up until the schema
// disagrees.
//
// Deliberately NOT a per-technology command. There is no `forge db dsn`
// here, because a project may have two databases, or none, or reach its
// store over something that is not a DSN at all. forge's business is that
// the environment declares its configuration and forge resolves it; naming
// DATABASE_URL in a command signature would pin a convention that is the
// project's to choose. Print what is declared and let the caller select:
//
//	forge env config dev --json | jq -r '.workloads[].env.DATABASE_URL // empty'

package cli

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"text/tabwriter"

	"github.com/spf13/cobra"
)

// envConfigWorkload is one workload's resolved configuration.
type envConfigWorkload struct {
	Name string            `json:"name"`
	Kind string            `json:"kind"` // service | job | frontend | operator | cronjob
	Env  map[string]string `json:"env,omitempty"`
}

// envConfigReport is the whole environment, ordered for stable output.
type envConfigReport struct {
	Env       string              `json:"env"`
	Workloads []envConfigWorkload `json:"workloads"`
}

func newEnvConfigCmd() *cobra.Command {
	var jsonOut bool
	var nameFilter string

	cmd := &cobra.Command{
		Use:   "config <env>",
		Short: "Print the resolved configuration deploy/kcl/<env>/ hands each workload",
		Args:  cobra.ExactArgs(1),
		Long: `Print the environment variables ` + "`" + `deploy/kcl/<env>/` + "`" + ` resolves for every
workload — the same values ` + Name() + ` env up passes to each process.

This is the readback for "what is this environment actually configured with":
which database, which broker, which upstream. Ports that were resolved at
launch are reported as launched, so the values match the running stack rather
than a fresh render's guess.

It prints whatever the environment declares. ` + Name() + ` has no opinion about
which variables a project uses, so pick the one you want:

  ` + Name() + ` env config dev
  ` + Name() + ` env config dev --workload api
  ` + Name() + ` env config dev --json | jq -r '.workloads[].env.DATABASE_URL // empty'

  # connect to whatever this project calls its database
  psql "$(` + Name() + ` env config dev --json | jq -r '.workloads[].env.DATABASE_URL // empty' | head -1)"`,
		RunE: func(cmd *cobra.Command, args []string) error {
			envName := args[0]
			projectDir := projectDirForKCL()

			entities, err := RenderKCL(cmd.Context(), projectDir, envName)
			if err != nil {
				return fmt.Errorf("read deploy/kcl/%s/: %w", envName, err)
			}
			if entities == nil {
				return fmt.Errorf("deploy/kcl/%s/ rendered no workloads", envName)
			}

			report := envConfigReport{
				Env:       envName,
				Workloads: collectEnvConfigWorkloads(entities),
			}
			// Launch-time values win over a fresh render: an ephemeral port
			// that moved when the stack came up lives in the persist and
			// nowhere in the KCL, so reporting the render here would hand
			// back a coordinate nothing is listening on.
			overlayLaunchedEnv(&report, projectDir, envName)

			if nameFilter != "" {
				report.Workloads = filterEnvConfigWorkloads(report.Workloads, nameFilter)
				if len(report.Workloads) == 0 {
					return fmt.Errorf("environment %q declares no workload named %q", envName, nameFilter)
				}
			}

			if jsonOut {
				enc := json.NewEncoder(cmd.OutOrStdout())
				enc.SetIndent("", "  ")
				return enc.Encode(report)
			}
			return renderEnvConfigTable(cmd, report)
		},
	}

	cmd.Flags().BoolVar(&jsonOut, "json", false, "Emit machine-readable JSON")
	cmd.Flags().StringVar(&nameFilter, "workload", "", "Only this workload (service, job, frontend, operator or cronjob)")
	return cmd
}

// collectEnvConfigWorkloads flattens every workload kind into one ordered
// list. Every kind that carries env vars is included: a value a workload is
// launched with is worth reading back regardless of which lowering runs it.
func collectEnvConfigWorkloads(e *KCLEntities) []envConfigWorkload {
	var out []envConfigWorkload
	add := func(name, kind string, vars []KCLEnvVar) {
		out = append(out, envConfigWorkload{Name: name, Kind: kind, Env: envVarMap(vars)})
	}
	for _, s := range e.Services {
		// A service carries config in two places: the top-level block (the
		// merged per-env set every lowering sees) and, for a host process,
		// the host deploy block that `forge env up` layers on at launch.
		// Report the union, host last, because that is the order the
		// launched process resolves them in.
		vars := append([]KCLEnvVar{}, s.EnvVars...)
		if s.Deploy.Host != nil {
			vars = append(vars, s.Deploy.Host.EnvVars...)
		}
		add(s.Name, "service", vars)
	}
	for _, j := range e.Jobs {
		add(j.Name, "job", j.EnvVars)
	}
	for _, f := range e.Frontends {
		add(f.Name, "frontend", f.EffectiveEnvVars())
	}
	for _, o := range e.Operators {
		add(o.Name, "operator", o.EnvVars)
	}
	for _, c := range e.CronJobs {
		add(c.Name, "cronjob", c.EnvVars)
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Kind != out[j].Kind {
			return out[i].Kind < out[j].Kind
		}
		return out[i].Name < out[j].Name
	})
	return out
}

// envVarMap converts the rendered slice to a map. Later entries win, which
// matches the name-keyed LAST-wins merge the KCL layer performs.
func envVarMap(vars []KCLEnvVar) map[string]string {
	if len(vars) == 0 {
		return nil
	}
	m := make(map[string]string, len(vars))
	for _, v := range vars {
		m[v.Name] = v.Value
	}
	return m
}

// overlayLaunchedEnv replaces rendered values with the ones the running
// stack was actually launched with, where a persist exists.
//
// Only variables the launch recorded are overlaid; everything else keeps its
// rendered value, so a stopped stack still reports a complete environment.
func overlayLaunchedEnv(report *envConfigReport, projectDir, envName string) {
	st := loadResolvedEnv(projectIDForDir(projectDir), envName)
	if st == nil || st.DatabaseURL == "" {
		return
	}
	// The persist records the DSN the stack dialed. Overlay it only where
	// the render already declared the same variable — this reports what
	// launched, it does not invent configuration a workload never had.
	for i := range report.Workloads {
		if _, declared := report.Workloads[i].Env["DATABASE_URL"]; declared {
			report.Workloads[i].Env["DATABASE_URL"] = st.DatabaseURL
		}
	}
}

// filterEnvConfigWorkloads narrows to one workload by name.
func filterEnvConfigWorkloads(in []envConfigWorkload, name string) []envConfigWorkload {
	var out []envConfigWorkload
	for _, w := range in {
		if strings.EqualFold(w.Name, name) {
			out = append(out, w)
		}
	}
	return out
}

// renderEnvConfigTable writes the human view: one block per workload, its
// variables sorted by name.
func renderEnvConfigTable(cmd *cobra.Command, report envConfigReport) error {
	out := cmd.OutOrStdout()
	if len(report.Workloads) == 0 {
		fmt.Fprintf(out, "deploy/kcl/%s/ declares no workloads.\n", report.Env)
		return nil
	}
	for i, w := range report.Workloads {
		if i > 0 {
			fmt.Fprintln(out)
		}
		fmt.Fprintf(out, "%s (%s)\n", w.Name, w.Kind)
		if len(w.Env) == 0 {
			fmt.Fprintln(out, "  (no configuration declared)")
			continue
		}
		names := make([]string, 0, len(w.Env))
		for k := range w.Env {
			names = append(names, k)
		}
		sort.Strings(names)

		tw := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
		for _, n := range names {
			fmt.Fprintf(tw, "  %s\t%s\n", n, w.Env[n])
		}
		if err := tw.Flush(); err != nil {
			return err
		}
	}
	return nil
}
