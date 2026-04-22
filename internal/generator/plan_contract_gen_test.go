package generator

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/reliant-labs/forge/internal/config"
)

func TestGeneratePlanContract_FullExample(t *testing.T) {
	dir := t.TempDir()

	methods := []config.PlanMethod{
		{Name: "CreateInvoice", Args: "ctx context.Context, req CreateInvoiceRequest", Returns: "*Invoice, error"},
		{Name: "GetInvoice", Args: "ctx context.Context, invoiceID string", Returns: "*Invoice, error"},
	}
	types := []config.PlanType{
		{
			Name: "Invoice",
			Fields: []config.PlanTypeField{
				{Name: "ID", Type: "string"},
				{Name: "PatientID", Type: "string"},
				{Name: "Amount", Type: "int64"},
				{Name: "Status", Type: "InvoiceStatus"},
			},
		},
		{Name: "InvoiceStatus"}, // no fields → enum placeholder
	}

	if err := GeneratePlanContract(dir, "billing", methods, types); err != nil {
		t.Fatalf("GeneratePlanContract failed: %v", err)
	}

	data, err := os.ReadFile(filepath.Join(dir, "contract.go"))
	if err != nil {
		t.Fatalf("read contract.go: %v", err)
	}
	content := string(data)

	// Package declaration
	if !strings.Contains(content, "package billing") {
		t.Error("missing package declaration")
	}

	// Import context
	if !strings.Contains(content, `"context"`) {
		t.Error("missing context import")
	}

	// Interface methods
	if !strings.Contains(content, "CreateInvoice(ctx context.Context, req CreateInvoiceRequest) (*Invoice, error)") {
		t.Errorf("missing CreateInvoice method, got:\n%s", content)
	}
	if !strings.Contains(content, "GetInvoice(ctx context.Context, invoiceID string) (*Invoice, error)") {
		t.Errorf("missing GetInvoice method, got:\n%s", content)
	}

	// Struct fields with JSON tags
	if !strings.Contains(content, `ID string`) {
		t.Error("missing ID field")
	}
	if !strings.Contains(content, `json:"id"`) {
		t.Error("missing id JSON tag")
	}
	if !strings.Contains(content, `json:"patient_id"`) {
		t.Error("missing patient_id JSON tag")
	}
	if !strings.Contains(content, `json:"amount"`) {
		t.Error("missing amount JSON tag")
	}

	// Enum placeholder type
	if !strings.Contains(content, "type InvoiceStatus string") {
		t.Errorf("missing enum placeholder type, got:\n%s", content)
	}
}

func TestGeneratePlanContract_CustomJSONTag(t *testing.T) {
	dir := t.TempDir()

	types := []config.PlanType{
		{
			Name: "User",
			Fields: []config.PlanTypeField{
				{Name: "ID", Type: "string", JSON: "user_id"},
			},
		},
	}

	if err := GeneratePlanContract(dir, "accounts", nil, types); err != nil {
		t.Fatalf("GeneratePlanContract failed: %v", err)
	}

	data, err := os.ReadFile(filepath.Join(dir, "contract.go"))
	if err != nil {
		t.Fatalf("read contract.go: %v", err)
	}

	if !strings.Contains(string(data), `json:"user_id"`) {
		t.Errorf("custom JSON tag not used, got:\n%s", string(data))
	}
}

func TestGeneratePlanContract_NoMethodsOnlyTypes(t *testing.T) {
	dir := t.TempDir()

	types := []config.PlanType{
		{
			Name: "Config",
			Fields: []config.PlanTypeField{
				{Name: "Timeout", Type: "time.Duration"},
			},
		},
	}

	if err := GeneratePlanContract(dir, "settings", nil, types); err != nil {
		t.Fatalf("GeneratePlanContract failed: %v", err)
	}

	data, err := os.ReadFile(filepath.Join(dir, "contract.go"))
	if err != nil {
		t.Fatalf("read contract.go: %v", err)
	}
	content := string(data)

	// Should still have empty Service interface
	if !strings.Contains(content, "type Service interface") {
		t.Error("missing Service interface")
	}

	// Should import time
	if !strings.Contains(content, `"time"`) {
		t.Errorf("missing time import, got:\n%s", content)
	}
}

func TestStripPackagePrefix(t *testing.T) {
	tests := []struct {
		pkg, typ, want string
	}{
		{"billing", "BillingInvoice", "Invoice"},
		{"billing", "Invoice", "Invoice"},
		{"billing", "Billing", "Billing"},       // exact match, no trimming (would leave empty)
		{"billing", "BillingStatus", "Status"},
		{"auth", "AuthToken", "Token"},
		{"auth", "Authorization", "Authorization"}, // "orization" starts lowercase
	}

	for _, tt := range tests {
		got := stripPackagePrefix(tt.pkg, tt.typ)
		if got != tt.want {
			t.Errorf("stripPackagePrefix(%q, %q) = %q, want %q", tt.pkg, tt.typ, got, tt.want)
		}
	}
}

func TestGeneratePlanContract_PackagePrefixStripping(t *testing.T) {
	dir := t.TempDir()

	types := []config.PlanType{
		{
			Name: "BillingInvoice",
			Fields: []config.PlanTypeField{
				{Name: "ID", Type: "string"},
			},
		},
	}

	if err := GeneratePlanContract(dir, "billing", nil, types); err != nil {
		t.Fatalf("GeneratePlanContract failed: %v", err)
	}

	data, err := os.ReadFile(filepath.Join(dir, "contract.go"))
	if err != nil {
		t.Fatalf("read contract.go: %v", err)
	}
	content := string(data)

	// Should have stripped the "Billing" prefix
	if !strings.Contains(content, "type Invoice struct") {
		t.Errorf("expected type Invoice struct (prefix stripped), got:\n%s", content)
	}
	if strings.Contains(content, "type BillingInvoice struct") {
		t.Errorf("should have stripped BillingInvoice → Invoice, got:\n%s", content)
	}
}

func TestCollectPlanImports(t *testing.T) {
	methods := []config.PlanMethod{
		{Name: "Do", Args: "ctx context.Context", Returns: "error"},
	}
	types := []config.PlanType{
		{
			Name: "Event",
			Fields: []config.PlanTypeField{
				{Name: "At", Type: "time.Time"},
			},
		},
	}

	imports := collectPlanImports(methods, types)

	hasContext, hasTime := false, false
	for _, imp := range imports {
		if imp == "context" {
			hasContext = true
		}
		if imp == "time" {
			hasTime = true
		}
	}
	if !hasContext {
		t.Error("expected context import")
	}
	if !hasTime {
		t.Error("expected time import")
	}
}
