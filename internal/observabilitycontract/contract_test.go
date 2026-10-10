package observabilitycontract

import (
	"strings"
	"testing"
)

func TestPinnedImagesAreDigestPinned(t *testing.T) {
	for name, image := range Images {
		if !strings.Contains(image, "@sha256:") || PinnedDigest(image) == "" {
			t.Errorf("%s is not digest pinned: %s", name, image)
		}
		if strings.Contains(image, ":latest") {
			t.Errorf("%s uses latest: %s", name, image)
		}
	}
	if !strings.Contains(Images["hyperdx"], ":"+HyperDXVersion+"@") || !strings.Contains(Images["collector"], ":"+HyperDXVersion+"@") {
		t.Fatal("HyperDX and collector must use the same semantic version")
	}
}

func TestComposeSafety(t *testing.T) {
	if strings.Contains(ComposeTemplate, "latest") {
		t.Fatal("compose template must not use latest")
	}
	for _, port := range []string{"${CH_PORT}", "${HDX_PORT}", "${GRPC_PORT}"} {
		if !strings.Contains(ComposeTemplate, "127.0.0.1:"+port) {
			t.Errorf("%s is not loopback bound", port)
		}
	}
	if !strings.Contains(ComposeTemplate, "test-only") {
		t.Fatal("compose template must document empty ClickHouse credentials as test-only")
	}
}

func TestNormalizeSchemaAndHash(t *testing.T) {
	first := "{\"type\":\"String\",\"name\":\"ServiceName\"}\n{\"name\":\"TraceId\",\"type\":\"FixedString(32)\"}\n"
	second := " {\"name\":\"TraceId\",\"type\":\"FixedString(32)\"}\n{\"type\":\"String\",\"name\":\"ServiceName\"}\n"
	if NormalizeSchema(first) != NormalizeSchema(second) || SchemaHash(first) != SchemaHash(second) {
		t.Fatal("schema normalization must make order and JSON key order irrelevant")
	}
}
