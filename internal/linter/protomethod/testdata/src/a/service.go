package testv1

import (
	"context"
	"net/http"
)

// --- Case 1: Type name matches interface prefix (testServiceHandler matches TestServiceHandler) ---

type testServiceHandler struct {
	UnimplementedTestServiceHandler
}

// These implement the proto interface — should NOT be flagged.
func (s *testServiceHandler) CreateItem(ctx context.Context, req *CreateItemRequest) (*Item, error) {
	return nil, nil
}

func (s *testServiceHandler) GetItem(ctx context.Context, req *GetItemRequest) (*Item, error) {
	return nil, nil
}

// Scaffolded framework methods — should NOT be flagged.
func (s *testServiceHandler) Name() string { return "test" }

func (s *testServiceHandler) Register(mux *http.ServeMux) {}

func (s *testServiceHandler) RegisterHTTP(mux *http.ServeMux) {}

// Exported method NOT in the proto interface — should be flagged.
func (s *testServiceHandler) BadMethod() string { // want "exported method testServiceHandler.BadMethod does not implement a proto service interface"
	return "bad"
}

// Another exported method NOT in the interface — should be flagged.
func (s *testServiceHandler) AnotherBadMethod(id int) (string, error) { // want "exported method testServiceHandler.AnotherBadMethod does not implement a proto service interface"
	return "", nil
}

// REST handler on a proto service type — allowed because it has the HTTP handler signature.
func (s *testServiceHandler) HandleCheckout(w http.ResponseWriter, r *http.Request) {
}

// Unexported methods are always fine.
func (s *testServiceHandler) unexportedMethod() {
}

// Inline annotation escape hatch — should NOT be flagged.
func (s *testServiceHandler) InlineAnnotation() {} //forge:allow

// --- Case 2: Type name does NOT match interface prefix (Service vs TestServiceHandler) ---
// This is the real-world pattern: forge scaffolds a type called "Service"
// that implements "APIServiceHandler".

type Service struct {
	UnimplementedTestServiceHandler
}

// These implement the proto interface — should NOT be flagged.
func (s *Service) CreateItem(ctx context.Context, req *CreateItemRequest) (*Item, error) {
	return nil, nil
}

func (s *Service) GetItem(ctx context.Context, req *GetItemRequest) (*Item, error) {
	return nil, nil
}

// Scaffolded framework methods — should NOT be flagged.
func (s *Service) Name() string { return "svc" }

func (s *Service) Register(mux *http.ServeMux) {}

func (s *Service) RegisterHTTP(mux *http.ServeMux) {}

// HTTP handler on a proto service type — allowed because it has the HTTP handler signature.
func (s *Service) HandleBillingWebhook(w http.ResponseWriter, r *http.Request) {
}

// Exported method with wrong signature — NOT an HTTP handler, should be flagged.
func (s *Service) HandleBadEndpoint(w http.ResponseWriter) { // want "exported method Service.HandleBadEndpoint does not implement a proto service interface"
}

// Exported method with no params — still flagged.
func (s *Service) ProcessWebhook() string { // want "exported method Service.ProcessWebhook does not implement a proto service interface"
	return ""
}

//forge:allow — custom exported method needed for legacy integration
func (s *Service) LegacyExport() string {
	return "legacy"
}

// --- Case 3: Type that does NOT implement any proto interface ---

type OtherType struct{}

func (o *OtherType) SomeMethod() { // Not flagged: OtherType doesn't implement a proto handler.
}

// --- Case 4: Constructor functions — should NOT be flagged ---

func New() *Service { return &Service{} }

func NewAuthorizer() *Authorizer { return &Authorizer{} }

// --- Case 5: Standalone exported non-constructor function — should be flagged ---

func ExportedFunction() { // want "exported function ExportedFunction is not allowed"
}

// Unexported standalone functions are fine.
func helperFunction(id string) error {
	return nil
}

// --- Case 6: Authorizer type (not a proto handler) ---

type Authorizer struct{}

func (a *Authorizer) CanAccess(ctx context.Context, procedure string) error { // Not flagged: Authorizer doesn't implement a proto handler.
	return nil
}
