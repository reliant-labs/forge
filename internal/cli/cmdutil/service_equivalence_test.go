package cmdutil

import (
	"strings"
	"testing"
)

func TestServiceDirectoryEquivalentSpellings(t *testing.T) {
	for _, tc := range []struct{ service, dir string }{
		{"ApiKeyService", "api_key"}, {"ApiKeyService", "apikey"},
		{"HttpProxyService", "http_proxy"}, {"UrlShortenerService", "url_shortener"},
		{"OauthClientService", "oauth_client"}, {"WorkOrderService", "workorder"},
	} {
		if err := ValidateServiceDirConsistency(tc.service, tc.dir); err != nil {
			t.Errorf("%s/%s: %v", tc.service, tc.dir, err)
		}
	}
}

func TestServiceDirectoryMismatchHintsChangeState(t *testing.T) {
	err := ValidateServiceDirConsistency("ApiKeyService", "orders")
	if err == nil {
		t.Fatal("unrelated directory accepted")
	}
	for _, noOp := range []string{`rename the proto service to "ApiKeyService"`, `rename the directory to "orders"`} {
		if strings.Contains(err.Error(), noOp) {
			t.Errorf("no-op hint: %v", err)
		}
	}
}
