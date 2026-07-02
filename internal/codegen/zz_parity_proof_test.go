package codegen

// zz_parity_proof_test.go is a NON-HERMETIC parity proof (Phase 3 acceptance
// gate). It reads control-plane's real proto config field set + per-env
// config.<env>.yaml, renders BOTH the typed KCL path (schema + projection +
// migrated config.k, executed via the `kcl` binary) and the current Go
// projector (renderDeployConfigKCL, also executed via `kcl`), then compares
// the two [EnvVar]+ConfigMap outputs field-by-field.
//
// It is guarded by RUN_PARITY=1 so it never runs in the normal suite (it
// shells to kcl and reads a sibling repo by absolute path). Run with:
//
//	RUN_PARITY=1 go test ./internal/codegen/ -run TestParityControlPlane -v

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"

	cfgloader "github.com/reliant-labs/forge/internal/config"
)

const (
	cpDir     = "/Users/user/src/reliant-labs/control-plane"
	forgeKCL  = "/Users/user/src/reliant-labs/forge/kcl"
	cpProject = "control-plane"
)

// controlPlaneFields mirrors proto/config/v1/config.proto in source order.
// Durations use ProtoType "google.protobuf.Duration" (maps to str, same as the
// descriptor extractor's message/string duration-leaf shape).
func controlPlaneFields() []ConfigField {
	s := func(name, env, def string) ConfigField {
		return ConfigField{Name: name, ProtoType: "string", GoType: "string", EnvVar: env, DefaultValue: def}
	}
	i32 := func(name, env, def string) ConfigField {
		return ConfigField{Name: name, ProtoType: "int32", GoType: "int32", EnvVar: env, DefaultValue: def}
	}
	i64 := func(name, env, def string) ConfigField {
		return ConfigField{Name: name, ProtoType: "int64", GoType: "int64", EnvVar: env, DefaultValue: def}
	}
	b := func(name, env, def string) ConfigField {
		return ConfigField{Name: name, ProtoType: "bool", GoType: "bool", EnvVar: env, DefaultValue: def}
	}
	dur := func(name, env, def string) ConfigField {
		return ConfigField{Name: name, ProtoType: "google.protobuf.Duration", GoType: "time.Duration", EnvVar: env, DefaultValue: def}
	}
	sec := func(name, env string) ConfigField {
		return ConfigField{Name: name, ProtoType: "string", GoType: "string", EnvVar: env, Sensitive: true}
	}
	return []ConfigField{
		i32("port", "PORT", "8080"),
		s("log_level", "LOG_LEVEL", "info"),
		s("database_url", "DATABASE_URL", ""),
		s("cors_origins", "CORS_ORIGINS", ""),
		b("cors_allow_credentials", "CORS_ALLOW_CREDENTIALS", "false"),
		s("tls_cert_path", "TLS_CERT_PATH", ""),
		s("tls_key_path", "TLS_KEY_PATH", ""),
		dur("pre_stop_delay", "PRE_STOP_DELAY", "5s"),
		dur("shutdown_timeout", "SHUTDOWN_TIMEOUT", "30s"),
		s("log_format", "LOG_FORMAT", "json"),
		b("auto_migrate", "AUTO_MIGRATE", "false"),
		s("environment", "ENVIRONMENT", "production"),
		i32("rate_limit_rps", "RATE_LIMIT_RPS", "100"),
		i32("rate_limit_burst", "RATE_LIMIT_BURST", "200"),
		i32("db_max_open_conns", "DB_MAX_OPEN_CONNS", ""),
		i32("db_max_idle_conns", "DB_MAX_IDLE_CONNS", ""),
		dur("db_conn_max_idle_time", "DB_CONN_MAX_IDLE_TIME", ""),
		dur("db_conn_max_lifetime", "DB_CONN_MAX_LIFETIME", ""),
		s("pprof_addr", "PPROF_ADDR", ""),
		b("security_headers_enabled", "SECURITY_HEADERS_ENABLED", "true"),
		s("otlp_endpoint", "OTEL_EXPORTER_OTLP_ENDPOINT", ""),
		s("service_name", "OTEL_SERVICE_NAME", "unknown"),
		s("deploy_env", "DEPLOY_ENV", ""),
		s("email_from", "EMAIL_FROM", "Reliant <noreply@reliant.dev>"),
		s("app_url", "APP_URL", "http://localhost:3000"),
		s("litellm_url", "LITELLM_URL", ""),
		s("nats_url", "NATS_URL", ""),
		s("nats_user", "NATS_USER", ""),
		s("workspace_controller_url", "WORKSPACE_CONTROLLER_URL", ""),
		s("github_client_id", "GITHUB_CLIENT_ID", ""),
		s("github_redirect_uri", "GITHUB_REDIRECT_URI", ""),
		s("proxy_host", "PROXY_HOST", ""),
		s("workspace_base_domain", "WORKSPACE_BASE_DOMAIN", ""),
		s("reliant_api_url", "RELIANT_API_URL", ""),
		s("daemon_image", "DAEMON_IMAGE", ""),
		s("server_url", "SERVER_URL", ""),
		s("gateway_url", "GATEWAY_URL", ""),
		s("allowed_redirect_hosts", "ALLOWED_REDIRECT_HOSTS", ""),
		s("supabase_jwt_issuer", "SUPABASE_JWT_ISSUER", ""),
		s("runtime_class_name", "RUNTIME_CLASS_NAME", "gvisor"),
		s("daemon_runtime_class", "DAEMON_RUNTIME_CLASS", ""),
		s("default_storage_class", "DEFAULT_STORAGE_CLASS", "workspace-ssd"),
		s("daemon_storage_class", "DAEMON_STORAGE_CLASS", ""),
		s("run_as_non_root", "RUN_AS_NON_ROOT", "true"),
		i64("workspace_fs_group", "WORKSPACE_FS_GROUP", ""),
		i64("workspace_run_as_user", "WORKSPACE_RUN_AS_USER", ""),
		s("daemon_dns_servers", "DAEMON_DNS_SERVERS", "8.8.8.8,8.8.4.4"),
		s("daemon_cluster_id", "DAEMON_CLUSTER_ID", ""),
		s("cluster_configs", "CLUSTER_CONFIGS", ""),
		s("control_plane_pod_cidr", "CONTROL_PLANE_POD_CIDR", ""),
		s("egress_proxy_url", "EGRESS_PROXY_URL", ""),
		s("image_pull_secret_name", "IMAGE_PULL_SECRET_NAME", ""),
		s("image_pull_secret_path", "IMAGE_PULL_SECRET_PATH", ""),
		s("control_plane_namespace", "CONTROL_PLANE_NAMESPACE", "reliant"),
		s("daemon_dial_out", "DAEMON_DIAL_OUT", ""),
		s("reliant_gateway_url", "RELIANT_GATEWAY_URL", ""),
		dur("idle_timeout", "IDLE_TIMEOUT", ""),
		s("proxy_base_domain", "PROXY_BASE_DOMAIN", "workspaces.reliantapi.com"),
		s("proxy_authz_mode", "PROXY_AUTHZ_MODE", ""),
		s("proxy_authz_url", "PROXY_AUTHZ_URL", ""),
		i32("metrics_port", "METRICS_PORT", "9090"),
		s("control_plane_cluster_id", "CONTROL_PLANE_CLUSTER_ID", ""),
		dur("proxy_port_access_cache_ttl", "PROXY_PORT_ACCESS_CACHE_TTL", ""),
		sec("resend_api_key", "RESEND_API_KEY"),
		sec("litellm_master_key", "LITELLM_MASTER_KEY"),
		sec("nats_password", "NATS_PASSWORD"),
		sec("stripe_secret_key", "STRIPE_SECRET_KEY"),
		sec("stripe_webhook_secret", "STRIPE_WEBHOOK_SECRET"),
		sec("litellm_webhook_secret", "LITELLM_WEBHOOK_SECRET"),
		sec("github_client_secret", "GITHUB_CLIENT_SECRET"),
		sec("oauth_state_secret", "OAUTH_STATE_SECRET"),
		sec("proxy_session_secret", "PROXY_SESSION_SECRET"),
		{Name: "internal_service_secret", ProtoType: "string", GoType: "string", EnvVar: "INTERNAL_SERVICE_SECRET", Sensitive: true, Required: true},
	}
}

type envVarJSON struct {
	Name         string `json:"name"`
	Value        string `json:"value"`
	SecretRef    string `json:"secret_ref"`
	SecretKey    string `json:"secret_key"`
	ConfigMapRef string `json:"config_map_ref"`
	ConfigMapKey string `json:"config_map_key"`
}

type configMapJSON struct {
	Name string            `json:"name"`
	Data map[string]string `json:"data"`
}

func kclRun(t *testing.T, dir string, out any) {
	t.Helper()
	cmd := exec.Command("kcl", "run", ".", "--format", "json")
	cmd.Dir = dir
	b, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("kcl run %s failed: %v\n%s", dir, err, b)
	}
	if err := json.Unmarshal(b, out); err != nil {
		t.Fatalf("parse kcl json from %s: %v\n%s", dir, err, b)
	}
}

func writeMod(t *testing.T, dir string) {
	t.Helper()
	mod := "[package]\nname = \"parity\"\nedition = \"v0.11.0\"\nversion = \"0.1.0\"\n\n[dependencies]\nforge = { path = \"" + forgeKCL + "\" }\n"
	if err := os.WriteFile(filepath.Join(dir, "kcl.mod"), []byte(mod), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestParityControlPlane(t *testing.T) {
	if os.Getenv("RUN_PARITY") != "1" {
		t.Skip("set RUN_PARITY=1 to run the non-hermetic control-plane parity proof")
	}
	fields := controlPlaneFields()
	defaults := map[string]ConfigField{}
	for _, f := range fields {
		defaults[f.EnvVar] = f
	}

	schema, err := GenerateConfigSchemaKCL(fields, cpProject)
	if err != nil {
		t.Fatal(err)
	}
	proj, err := GenerateConfigProjectionKCL(fields)
	if err != nil {
		t.Fatal(err)
	}

	for _, env := range []string{"dev", "dev-k8s", "e2e", "staging", "preprod", "prod"} {
		t.Run(env, func(t *testing.T) {
			envCfg, err := cfgloader.LoadEnvironmentConfig(cpDir, env)
			if err != nil {
				t.Fatalf("load config.%s.yaml: %v", env, err)
			}
			cmName := cpProject + "-" + env + "-config"

			// --- typed path ---
			typedDir := t.TempDir()
			writeMod(t, typedDir)
			configK, err := GenerateConfigKFromYAML(fields, envCfg, cpProject)
			if err != nil {
				t.Fatal(err)
			}
			// The schema module MUST be named config_schema.k — projection.k
			// and config.k `import config_schema` by that stem.
			writeK(t, typedDir, "config_schema.k", schema)
			writeK(t, typedDir, "projection.k", proj)
			writeK(t, typedDir, "config.k", configK)
			// The projection now lowers to the agnostic env MAP; project it to
			// [forge.EnvVar] via env_project for the list-shaped Go-projector
			// parity comparison.
			mainK := "import forge\n" +
				"envvars: [forge.EnvVar] = forge.env_project(appConfigEnvMap(app_config, \"" + cmName + "\"))\n" +
				"configmap: forge.ConfigMap = appConfigConfigMap(app_config, \"" + cmName + "\")\n"
			writeK(t, typedDir, "main.k", mainK)
			var typed struct {
				EnvVars   []envVarJSON  `json:"envvars"`
				ConfigMap configMapJSON `json:"configmap"`
			}
			kclRun(t, typedDir, &typed)

			// --- Go projector ground truth ---
			if err := validateDeployConfig(DeployConfigGenInput{ProjectName: cpProject, EnvName: env, Fields: fields, EnvConfig: envCfg}); err != nil {
				t.Fatalf("go projector validate: %v", err)
			}
			body := renderDeployConfigKCL(DeployConfigGenInput{ProjectName: cpProject, EnvName: env, Fields: fields, EnvConfig: envCfg})
			goDir := t.TempDir()
			writeMod(t, goDir)
			writeK(t, goDir, "config_gen.k", body)
			writeK(t, goDir, "main.k", "import forge\nenvvars: [forge.EnvVar] = APP_ENV\nconfigmap: [forge.ConfigMap] = CONFIG_MAPS\n")
			var goOut struct {
				EnvVars   []envVarJSON    `json:"envvars"`
				ConfigMap []configMapJSON `json:"configmap"`
			}
			kclRun(t, goDir, &goOut)

			compareEnv(t, env, typed.EnvVars, typed.ConfigMap, goOut.EnvVars, goOut.ConfigMap, defaults)
		})
	}
}

func writeK(t *testing.T, dir, name, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// compareEnv applies the Phase-3 parity rules and logs a full report.
func compareEnv(t *testing.T, env string, tEnv []envVarJSON, tCM configMapJSON, gEnv []envVarJSON, gCM []configMapJSON, defaults map[string]ConfigField) {
	typedByName := map[string]envVarJSON{}
	for _, e := range tEnv {
		typedByName[e.Name] = e
	}
	goByName := map[string]envVarJSON{}
	for _, e := range gEnv {
		goByName[e.Name] = e
	}

	var mismatches, extras, overlaps []string

	// Every Go-projector entry must appear identically in the typed output.
	goNames := make([]string, 0, len(goByName))
	for n := range goByName {
		goNames = append(goNames, n)
	}
	sort.Strings(goNames)
	for _, n := range goNames {
		g := goByName[n]
		tv, ok := typedByName[n]
		if !ok {
			mismatches = append(mismatches, "MISSING in typed: "+n+" (go had "+describe(g)+")")
			continue
		}
		if g.SecretRef != tv.SecretRef || g.SecretKey != tv.SecretKey ||
			g.ConfigMapRef != tv.ConfigMapRef || g.ConfigMapKey != tv.ConfigMapKey {
			mismatches = append(mismatches, "DIFF "+n+": go="+describe(g)+" typed="+describe(tv))
			continue
		}
		overlaps = append(overlaps, n+" -> "+describe(g))
	}

	// Every typed-only entry must be a NON-sensitive (config_map) field.
	typedNames := make([]string, 0, len(typedByName))
	for n := range typedByName {
		typedNames = append(typedNames, n)
	}
	sort.Strings(typedNames)
	for _, n := range typedNames {
		if _, ok := goByName[n]; ok {
			continue
		}
		tv := typedByName[n]
		if tv.SecretRef != "" {
			mismatches = append(mismatches, "EXTRA typed SECRET entry (must never happen): "+n+" "+describe(tv))
			continue
		}
		extras = append(extras, n)
	}

	// ConfigMap overlap: every Go data key must match the typed value.
	goData := map[string]string{}
	if len(gCM) > 0 {
		goData = gCM[0].Data
	}
	goDataKeys := make([]string, 0, len(goData))
	for k := range goData {
		goDataKeys = append(goDataKeys, k)
	}
	sort.Strings(goDataKeys)
	for _, k := range goDataKeys {
		tvVal, ok := tCM.Data[k]
		if !ok {
			mismatches = append(mismatches, "ConfigMap key MISSING in typed: "+k)
			continue
		}
		if tvVal != goData[k] {
			mismatches = append(mismatches, "ConfigMap value DIFF "+k+": go="+strconv.Quote(goData[k])+" typed="+strconv.Quote(tvVal))
		}
	}

	// ConfigMap extras: typed-only keys must equal the field's proto default
	// (bool compared case-insensitively — KCL str(bool) capitalizes).
	var cmExtraNote []string
	for k, v := range tCM.Data {
		if _, ok := goData[k]; ok {
			continue
		}
		f := defaults[k]
		want := f.DefaultValue
		if want == "" {
			want = typeZero(f)
		}
		got := v
		if f.ProtoType == "bool" {
			got, want = strings.ToLower(got), strings.ToLower(want)
		}
		if got != want {
			cmExtraNote = append(cmExtraNote, k+": typed="+strconv.Quote(v)+" protoDefault="+strconv.Quote(f.DefaultValue))
		}
	}

	t.Logf("=== ENV %s ===", env)
	t.Logf("overlap entries (identical in both): %d", len(overlaps))
	for _, o := range overlaps {
		t.Logf("  OVERLAP %s", o)
	}
	t.Logf("typed-only EXTRA non-sensitive env vars (allowed superset): %d", len(extras))
	t.Logf("  %s", strings.Join(extras, ", "))
	if len(cmExtraNote) > 0 {
		t.Logf("EXTRA configmap values NOT equal to proto default (investigate):")
		for _, n := range cmExtraNote {
			t.Logf("  %s", n)
		}
	}
	if len(mismatches) > 0 {
		for _, m := range mismatches {
			t.Errorf("[%s] MISMATCH: %s", env, m)
		}
	} else {
		t.Logf("PARITY OK for %s: all %d Go-projector entries matched; %d typed extras all non-sensitive", env, len(overlaps), len(extras))
	}
}

func describe(e envVarJSON) string {
	if e.SecretRef != "" {
		return "secret_ref=" + e.SecretRef + " secret_key=" + e.SecretKey
	}
	return "config_map_ref=" + e.ConfigMapRef + " config_map_key=" + e.ConfigMapKey
}

func typeZero(f ConfigField) string {
	switch kclTypeForProtoConfig(f) {
	case "int":
		return "0"
	case "float":
		return "0"
	case "bool":
		return "false"
	default:
		return ""
	}
}
