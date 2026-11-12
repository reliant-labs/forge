package deploytarget

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/reliant-labs/forge/pkg/deploy/v1alpha1"
)

type resourceCapsCaller struct {
	doc   string
	err   error
	calls []string
}

func (c *resourceCapsCaller) Call(_ context.Context, procedure string, _, out any) error {
	c.calls = append(c.calls, procedure)
	if procedure != ProcDeployCapabilities {
		return errors.New("unexpected write: " + procedure)
	}
	if c.err != nil {
		return c.err
	}
	return json.Unmarshal([]byte(c.doc), out)
}

func TestRequireHostedResourceFeatures(t *testing.T) {
	for _, tc := range []struct {
		name, doc string
		err       error
		ok        bool
	}{
		{"old server", `{}`, nil, false},
		{"empty list", `{"resourceFeatures":[]}`, nil, false},
		{"wrong channel", `{"bundleDocumentFeatures":["static-site-well-known"]}`, nil, false},
		{"supported", `{"resourceFeatures":["static-site-well-known"]}`, nil, true},
		{"unimplemented", "", errors.New("unimplemented"), false},
		{"unavailable", "", errors.New("network error"), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := &resourceCapsCaller{doc: tc.doc, err: tc.err}
			err := RequireHostedResourceFeatures(context.Background(), c, []string{ResourceFeatureStaticSiteWellKnown})
			if (err == nil) != tc.ok {
				t.Fatalf("err=%v", err)
			}
			if err != nil && !strings.Contains(err.Error(), "upgrade") {
				t.Fatalf("missing upgrade guidance: %v", err)
			}
			if len(c.calls) != 1 {
				t.Fatalf("calls=%v", c.calls)
			}
		})
	}
	c := &resourceCapsCaller{}
	if err := RequireHostedResourceFeatures(context.Background(), c, nil); err != nil || len(c.calls) != 0 {
		t.Fatalf("legacy group queried capabilities: %v %v", c.calls, err)
	}
}

func TestHostedProvider_OldServerRefusedBeforeAnyWrite(t *testing.T) {
	g := staticGroup("v2", map[string]string{staticSiteArtifact: digestB})
	g.Services[0].Hosted.Static.WellKnown = map[string]string{"assetlinks.json": "[]"}
	c := &resourceCapsCaller{doc: `{}`}
	p := HostedProvider{Client: c, RecordBundle: func(context.Context, string) (string, error) {
		t.Fatal("recorded a bundle before capabilities were checked")
		return "", nil
	}}
	if err := p.Deploy(context.Background(), g); err == nil || !strings.Contains(err.Error(), "upgrade") {
		t.Fatalf("err=%v", err)
	}
	if len(c.calls) != 1 || c.calls[0] != ProcDeployCapabilities {
		t.Fatalf("wrote before refusal: %v", c.calls)
	}
}

func TestRequiredHostedResourceFeatures(t *testing.T) {
	g := ServiceGroup{Services: []ResolvedService{{Name: "web", Hosted: &HostedWorkload{Tier: HostedTierStatic, Static: &v1alpha1.StaticSiteSpec{}}}}}
	if len(RequiredHostedResourceFeatures(g)) != 0 {
		t.Fatal("empty map requires a feature")
	}
	g.Services[0].Hosted.Static.WellKnown = map[string]string{"apple-app-site-association": "{}"}
	if got := RequiredHostedResourceFeatures(g); len(got) != 1 || got[0] != ResourceFeatureStaticSiteWellKnown {
		t.Fatalf("features=%v", got)
	}
}
