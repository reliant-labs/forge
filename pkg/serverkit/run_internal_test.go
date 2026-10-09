package serverkit

import "testing"

func TestObserveConfigProjectsTypedEnvironment(t *testing.T) {
	got := observeConfig(Config{
		ServiceName:    "api",
		ServiceVersion: "1.2.3",
		OTLPEndpoint:   "collector:4317",
		Environment:    "staging",
	}, "instance-1")

	if got.DeploymentEnvironment != "staging" {
		t.Errorf("DeploymentEnvironment = %q, want staging", got.DeploymentEnvironment)
	}
}
