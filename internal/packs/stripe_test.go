package packs

import (
	"bytes"
	"strings"
	"testing"
	"text/template"

	"github.com/reliant-labs/forge/internal/templates"
)

func TestStripePackManifest(t *testing.T) {
	p, err := LoadPack("stripe")
	if err != nil {
		t.Fatalf("LoadPack(stripe) error: %v", err)
	}

	if p.Name != "stripe" {
		t.Errorf("Name = %q, want %q", p.Name, "stripe")
	}
	if p.Version != "1.0.0" {
		t.Errorf("Version = %q, want %q", p.Version, "1.0.0")
	}
	if !strings.Contains(p.Description, "Stripe") {
		t.Errorf("Description should mention Stripe, got: %s", p.Description)
	}
	if !strings.Contains(p.Description, "webhook") {
		t.Errorf("Description should mention webhook, got: %s", p.Description)
	}

	// Check config section
	if p.Config.Section != "stripe" {
		t.Errorf("Config.Section = %q, want %q", p.Config.Section, "stripe")
	}

	// Check dependencies
	wantDeps := map[string]bool{
		"github.com/stripe/stripe-go/v82": false,
	}
	for _, dep := range p.Dependencies {
		if _, ok := wantDeps[dep]; ok {
			wantDeps[dep] = true
		}
	}
	for dep, found := range wantDeps {
		if !found {
			t.Errorf("missing dependency: %s", dep)
		}
	}

	// Check files reference the correct templates
	if len(p.Files) != 3 {
		t.Errorf("len(Files) = %d, want 3", len(p.Files))
	}
	templateNames := make(map[string]bool)
	for _, f := range p.Files {
		templateNames[f.Template] = true
	}
	for _, want := range []string{"stripe_client.go.tmpl", "stripe_webhook.go.tmpl", "stripe_entities.proto.tmpl"} {
		if !templateNames[want] {
			t.Errorf("files should include %s", want)
		}
	}

	// Check overwrite policy
	for _, f := range p.Files {
		if f.Overwrite != "once" {
			t.Errorf("File %s has overwrite %q, want %q", f.Template, f.Overwrite, "once")
		}
	}

	// Check generate hook
	if len(p.Generate) != 1 {
		t.Fatalf("len(Generate) = %d, want 1", len(p.Generate))
	}
	if p.Generate[0].Template != "stripe_webhook_routes.go.tmpl" {
		t.Errorf("Generate[0].Template = %q, want %q", p.Generate[0].Template, "stripe_webhook_routes.go.tmpl")
	}
}

func TestStripeTemplatesRender(t *testing.T) {
	p, err := LoadPack("stripe")
	if err != nil {
		t.Fatalf("LoadPack(stripe) error: %v", err)
	}

	data := map[string]any{
		"ModulePath":  "github.com/example/myapp",
		"ProjectName": "myapp",
		"PackConfig":  p.Config.Defaults,
	}

	// Test all file templates render without error
	allFiles := append(p.Files, p.Generate...)
	for _, f := range allFiles {
		t.Run(f.Template, func(t *testing.T) {
			tmplPath := "stripe/templates/" + f.Template
			tmplContent, err := packsFS.ReadFile(tmplPath)
			if err != nil {
				t.Fatalf("read template %s: %v", tmplPath, err)
			}

			tmpl, err := template.New(f.Template).Funcs(templates.FuncMap()).Parse(string(tmplContent))
			if err != nil {
				t.Fatalf("parse template %s: %v", f.Template, err)
			}

			var buf bytes.Buffer
			if err := tmpl.Execute(&buf, data); err != nil {
				t.Fatalf("execute template %s: %v", f.Template, err)
			}

			output := buf.String()
			if len(output) == 0 {
				t.Errorf("template %s produced empty output", f.Template)
			}
		})
	}
}

func TestStripeClientTemplateContent(t *testing.T) {
	tmplContent, err := packsFS.ReadFile("stripe/templates/stripe_client.go.tmpl")
	if err != nil {
		t.Fatalf("read template: %v", err)
	}

	content := string(tmplContent)

	checks := []string{
		"package stripe",
		"NewClient",
		"CreatePaymentIntent",
		"CapturePaymentIntent",
		"CancelPaymentIntent",
		"CreateCustomer",
		"UpdateCustomer",
		"DeleteCustomer",
		"CreateCheckoutSession",
		"stripe/stripe-go/v82",
	}
	for _, check := range checks {
		if !strings.Contains(content, check) {
			t.Errorf("stripe_client.go.tmpl should contain %q", check)
		}
	}
}

func TestStripeWebhookTemplateContent(t *testing.T) {
	tmplContent, err := packsFS.ReadFile("stripe/templates/stripe_webhook.go.tmpl")
	if err != nil {
		t.Fatalf("read template: %v", err)
	}

	content := string(tmplContent)

	checks := []string{
		"StripeEventHandler",
		"HandlePaymentIntentSucceeded",
		"HandlePaymentIntentFailed",
		"HandleCheckoutSessionCompleted",
		"HandleSubscriptionCreated",
		"HandleSubscriptionUpdated",
		"HandleSubscriptionDeleted",
		"HandleInvoicePaid",
		"HandleInvoicePaymentFailed",
		"webhook.ConstructEvent",
		"Stripe-Signature",
		"IdempotencyStore",
		"NoopEventHandler",
		"WebhookOption",
	}
	for _, check := range checks {
		if !strings.Contains(content, check) {
			t.Errorf("stripe_webhook.go.tmpl should contain %q", check)
		}
	}
}

func TestStripeEntitiesTemplateContent(t *testing.T) {
	tmplContent, err := packsFS.ReadFile("stripe/templates/stripe_entities.proto.tmpl")
	if err != nil {
		t.Fatalf("read template: %v", err)
	}

	content := string(tmplContent)

	checks := []string{
		"StripeCustomer",
		"StripePaymentIntent",
		"StripeSubscription",
		"StripeInvoice",
		"stripe_customer_id",
		"stripe_payment_intent_id",
		"stripe_subscription_id",
		"stripe_invoice_id",
		"current_period_start",
		"current_period_end",
		"amount_due",
		"amount_paid",
		"soft_delete: true",
		"forge.options.v1.entity_options",
		"forge.options.v1.field_options",
		"primary_key: true",
		"references:",
	}
	for _, check := range checks {
		if !strings.Contains(content, check) {
			t.Errorf("stripe_entities.proto.tmpl should contain %q", check)
		}
	}
}

func TestStripeWebhookRoutesTemplateContent(t *testing.T) {
	tmplContent, err := packsFS.ReadFile("stripe/templates/stripe_webhook_routes.go.tmpl")
	if err != nil {
		t.Fatalf("read template: %v", err)
	}

	content := string(tmplContent)

	checks := []string{
		"Code generated by forge generate (stripe pack). DO NOT EDIT.",
		"RegisterStripeWebhookRoutes",
		"/webhooks/stripe",
		"StripeEventHandler",
		"WebhookOption",
	}
	for _, check := range checks {
		if !strings.Contains(content, check) {
			t.Errorf("stripe_webhook_routes.go.tmpl should contain %q", check)
		}
	}
}

func TestStripeInListPacks(t *testing.T) {
	packs, err := ListPacks()
	if err != nil {
		t.Fatalf("ListPacks() error: %v", err)
	}

	found := false
	for _, p := range packs {
		if p.Name == "stripe" {
			found = true
			break
		}
	}
	if !found {
		t.Error("ListPacks() did not include stripe")
	}
}
