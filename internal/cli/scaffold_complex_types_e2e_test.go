//go:build e2e

package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestE2EScaffoldComplexTypes scaffolds a project, writes entity and service
// protos that exercise every complex proto type (enums, maps, repeated
// fields, nested messages, timestamps, soft-delete entities), then runs
// `forge generate` and `go build` to verify the full pipeline handles them.
//
// This is the regression test for the class of bugs where the code generators
// produced garbage for non-scalar field types (e.g. falling back to "string"
// for enums or maps, emitting invalid Go for repeated message fields).
//
// The test intentionally uses the options/v1 annotation format that
// `forge new` scaffolds for new projects.
func TestE2EScaffoldComplexTypes(t *testing.T) {
	forgeBin := buildforgeBinary(t)
	dir := t.TempDir()

	// Scaffold a project with two services.
	runCmd(t, dir, forgeBin,
		"new", "complexapp",
		"--mod", "example.com/complexapp",
		"--service", "api,inventory",
	)

	projectDir := filepath.Join(dir, "complexapp")
	assertPathExistsE2E(t, filepath.Join(projectDir, "forge.yaml"))

	// ── Write a DB entity proto with enums, nested messages, timestamps ──
	entityDir := filepath.Join(projectDir, "proto", "db", "v1")
	if err := os.MkdirAll(entityDir, 0o755); err != nil {
		t.Fatalf("mkdir entity dir: %v", err)
	}

	// Product entity — exercises: enum, repeated scalar, nested message,
	// map<string,string>, timestamps, soft-delete, indexes.
	productProto := `syntax = "proto3";

package complexapp.db.v1;

import "forge/options/v1/entity.proto";
import "forge/options/v1/field.proto";
import "google/protobuf/timestamp.proto";

option go_package = "example.com/complexapp/gen/db/v1;dbv1";

enum ProductStatus {
  PRODUCT_STATUS_UNSPECIFIED = 0;
  PRODUCT_STATUS_DRAFT = 1;
  PRODUCT_STATUS_ACTIVE = 2;
  PRODUCT_STATUS_ARCHIVED = 3;
}

// Nested message used as a field type.
message Dimension {
  double width = 1;
  double height = 2;
  double depth = 3;
  string unit = 4;
}

message Product {
  option (forge.options.v1.entity_options) = {
    table_name: "products"
    soft_delete: true
    timestamps: true
    indexes: [
      {
        name: "idx_products_status"
        fields: ["status"]
      },
      {
        name: "idx_products_sku"
        fields: ["sku"]
        unique: true
      }
    ]
  };

  string id = 1 [(forge.options.v1.field_options) = {
    primary_key: true
    not_null: true
  }];

  string name = 2 [(forge.options.v1.field_options) = {
    not_null: true
  }];

  string sku = 3 [(forge.options.v1.field_options) = {
    not_null: true
  }];

  ProductStatus status = 4 [(forge.options.v1.field_options) = {
    not_null: true
    default_value: "'PRODUCT_STATUS_DRAFT'"
  }];

  // Repeated scalar — list of tags stored as JSONB.
  repeated string tags = 5 [(forge.options.v1.field_options) = {
    column_type: "JSONB"
  }];

  // Map field — key-value metadata stored as JSONB.
  map<string, string> attributes = 6 [(forge.options.v1.field_options) = {
    column_type: "JSONB"
  }];

  // Nested message field stored as JSONB.
  Dimension dimensions = 7 [(forge.options.v1.field_options) = {
    column_type: "JSONB"
  }];

  int64 price_cents = 8 [(forge.options.v1.field_options) = {
    not_null: true
  }];

  int32 stock_count = 9;

  double weight_kg = 10;

  bool is_featured = 11;

  google.protobuf.Timestamp created_at = 12 [(forge.options.v1.field_options) = {
    not_null: true
    default_value: "NOW()"
  }];

  google.protobuf.Timestamp updated_at = 13 [(forge.options.v1.field_options) = {
    not_null: true
    default_value: "NOW()"
  }];

  google.protobuf.Timestamp deleted_at = 14;
}
`
	if err := os.WriteFile(filepath.Join(entityDir, "product.proto"), []byte(productProto), 0o644); err != nil {
		t.Fatalf("write product.proto: %v", err)
	}

	// Order entity — exercises: enum, repeated message, nested message.
	orderProto := `syntax = "proto3";

package complexapp.db.v1;

import "forge/options/v1/entity.proto";
import "forge/options/v1/field.proto";
import "google/protobuf/timestamp.proto";

option go_package = "example.com/complexapp/gen/db/v1;dbv1";

enum OrderStatus {
  ORDER_STATUS_UNSPECIFIED = 0;
  ORDER_STATUS_PENDING = 1;
  ORDER_STATUS_CONFIRMED = 2;
  ORDER_STATUS_SHIPPED = 3;
  ORDER_STATUS_DELIVERED = 4;
  ORDER_STATUS_CANCELLED = 5;
}

// LineItem is a nested message embedded in Order as a repeated field.
message LineItem {
  string product_id = 1;
  string product_name = 2;
  int32 quantity = 3;
  int64 unit_price_cents = 4;
}

// Address is a nested message embedded in Order.
message Address {
  string street = 1;
  string city = 2;
  string state = 3;
  string zip = 4;
  string country = 5;
}

message Order {
  option (forge.options.v1.entity_options) = {
    table_name: "orders"
    soft_delete: false
    timestamps: true
    indexes: [
      {
        name: "idx_orders_customer"
        fields: ["customer_id"]
      },
      {
        name: "idx_orders_status"
        fields: ["status", "created_at"]
      }
    ]
  };

  string id = 1 [(forge.options.v1.field_options) = {
    primary_key: true
    not_null: true
  }];

  string customer_id = 2 [(forge.options.v1.field_options) = {
    not_null: true
  }];

  OrderStatus status = 3 [(forge.options.v1.field_options) = {
    not_null: true
    default_value: "'ORDER_STATUS_PENDING'"
  }];

  // Repeated message field — line items stored as JSONB.
  repeated LineItem line_items = 4 [(forge.options.v1.field_options) = {
    column_type: "JSONB"
  }];

  // Nested message field — shipping address stored as JSONB.
  Address shipping_address = 5 [(forge.options.v1.field_options) = {
    column_type: "JSONB"
  }];

  int64 total_cents = 6 [(forge.options.v1.field_options) = {
    not_null: true
  }];

  string currency = 7 [(forge.options.v1.field_options) = {
    not_null: true
    default_value: "'USD'"
  }];

  string notes = 8;

  google.protobuf.Timestamp created_at = 9 [(forge.options.v1.field_options) = {
    not_null: true
    default_value: "NOW()"
  }];

  google.protobuf.Timestamp updated_at = 10 [(forge.options.v1.field_options) = {
    not_null: true
    default_value: "NOW()"
  }];
}
`
	if err := os.WriteFile(filepath.Join(entityDir, "order.proto"), []byte(orderProto), 0o644); err != nil {
		t.Fatalf("write order.proto: %v", err)
	}

	// ── Write a service proto that references the complex types ──────────
	// Overwrite the default api.proto with one that uses enums, repeated
	// fields, map fields, and nested messages in request/response types.
	apiProtoDir := filepath.Join(projectDir, "proto", "services", "api", "v1")
	apiProto := `syntax = "proto3";

package services.api.v1;

import "google/protobuf/field_mask.proto";
import "google/protobuf/timestamp.proto";
import "forge/options/v1/service.proto";
import "forge/options/v1/method.proto";

option go_package = "example.com/complexapp/gen/services/api/v1;apiv1";

enum SortOrder {
  SORT_ORDER_UNSPECIFIED = 0;
  SORT_ORDER_ASC = 1;
  SORT_ORDER_DESC = 2;
}

// Pagination is a nested message used in list requests.
message Pagination {
  int32 page_size = 1;
  string page_token = 2;
  SortOrder sort_order = 3;
  string sort_by = 4;
}

// Filter supports map-based dynamic filtering.
message Filter {
  map<string, string> field_equals = 1;
  repeated string tags = 2;
}

// ProductView is the API-facing representation of a product.
message ProductView {
  string id = 1;
  string name = 2;
  string sku = 3;
  string status = 4;
  repeated string tags = 5;
  map<string, string> attributes = 6;
  int64 price_cents = 7;
  bool is_featured = 8;
  google.protobuf.Timestamp created_at = 9;
  google.protobuf.Timestamp updated_at = 10;
}

service ApiService {
  option (forge.options.v1.service_options) = {
    name: "ApiService"
    version: "1.0.0"
    description: "API service with complex types"
  };

  rpc GetProduct(GetProductRequest) returns (GetProductResponse) {
    option (forge.options.v1.method_options) = {
      cache: { key_template: "Product:{request.id}" }
    };
  }

  rpc ListProducts(ListProductsRequest) returns (ListProductsResponse) {}

  rpc CreateProduct(CreateProductRequest) returns (CreateProductResponse) {
    option (forge.options.v1.method_options) = {
      auth_required: true
      idempotency_key: true
    };
  }

  rpc UpdateProduct(UpdateProductRequest) returns (UpdateProductResponse) {
    option (forge.options.v1.method_options) = {
      auth_required: true
    };
  }
}

message GetProductRequest {
  string id = 1;
}

message GetProductResponse {
  ProductView product = 1;
}

message ListProductsRequest {
  Pagination pagination = 1;
  Filter filter = 2;
  repeated string statuses = 3;
}

message ListProductsResponse {
  repeated ProductView products = 1;
  string next_page_token = 2;
  int32 total_count = 3;
}

message CreateProductRequest {
  string name = 1;
  string sku = 2;
  repeated string tags = 3;
  map<string, string> attributes = 4;
  int64 price_cents = 5;
}

message CreateProductResponse {
  ProductView product = 1;
}

message UpdateProductRequest {
  string id = 1;
  google.protobuf.FieldMask update_mask = 2;
  string name = 3;
  repeated string tags = 4;
  map<string, string> attributes = 5;
  int64 price_cents = 6;
  bool is_featured = 7;
}

message UpdateProductResponse {
  ProductView product = 1;
}
`
	if err := os.WriteFile(filepath.Join(apiProtoDir, "api.proto"), []byte(apiProto), 0o644); err != nil {
		t.Fatalf("write api.proto: %v", err)
	}

	// ── Write the inventory service proto with its own enum ──────────────
	inventoryProtoDir := filepath.Join(projectDir, "proto", "services", "inventory", "v1")
	inventoryProto := `syntax = "proto3";

package services.inventory.v1;

import "google/protobuf/timestamp.proto";
import "forge/options/v1/service.proto";
import "forge/options/v1/method.proto";

option go_package = "example.com/complexapp/gen/services/inventory/v1;inventoryv1";

enum AdjustmentType {
  ADJUSTMENT_TYPE_UNSPECIFIED = 0;
  ADJUSTMENT_TYPE_RESTOCK = 1;
  ADJUSTMENT_TYPE_SALE = 2;
  ADJUSTMENT_TYPE_RETURN = 3;
  ADJUSTMENT_TYPE_WRITE_OFF = 4;
}

message StockLevel {
  string product_id = 1;
  int32 available = 2;
  int32 reserved = 3;
  int32 incoming = 4;
  google.protobuf.Timestamp last_updated = 5;
}

message AdjustmentRecord {
  string id = 1;
  string product_id = 2;
  AdjustmentType type = 3;
  int32 quantity = 4;
  string reason = 5;
  google.protobuf.Timestamp created_at = 6;
}

service InventoryService {
  option (forge.options.v1.service_options) = {
    name: "InventoryService"
    version: "1.0.0"
    description: "Inventory management service"
  };

  rpc GetStockLevel(GetStockLevelRequest) returns (GetStockLevelResponse) {}
  rpc AdjustStock(AdjustStockRequest) returns (AdjustStockResponse) {
    option (forge.options.v1.method_options) = {
      auth_required: true
      idempotency_key: true
    };
  }
  rpc ListAdjustments(ListAdjustmentsRequest) returns (ListAdjustmentsResponse) {}
}

message GetStockLevelRequest {
  string product_id = 1;
}

message GetStockLevelResponse {
  StockLevel stock_level = 1;
}

message AdjustStockRequest {
  string product_id = 1;
  AdjustmentType type = 2;
  int32 quantity = 3;
  string reason = 4;
}

message AdjustStockResponse {
  StockLevel updated_stock = 1;
  AdjustmentRecord adjustment = 2;
}

message ListAdjustmentsRequest {
  string product_id = 1;
  repeated AdjustmentType types = 2;
  int32 page_size = 3;
  string page_token = 4;
}

message ListAdjustmentsResponse {
  repeated AdjustmentRecord adjustments = 1;
  string next_page_token = 2;
}
`
	if err := os.WriteFile(filepath.Join(inventoryProtoDir, "inventory.proto"), []byte(inventoryProto), 0o644); err != nil {
		t.Fatalf("write inventory.proto: %v", err)
	}

	// ── Generate code ────────────────────────────────────────────────────
	runCmd(t, projectDir, forgeBin, "generate")

	// ── Verify generated outputs exist ──────────────────────────────────
	mustExist := []string{
		// Proto-generated Go stubs
		"gen/services/api/v1/api.pb.go",
		"gen/services/inventory/v1/inventory.pb.go",
		"gen/db/v1/product.pb.go",
		"gen/db/v1/order.pb.go",

		// ORM-generated code for DB entities
		"gen/db/v1/product_product.pb.orm.go",
		"gen/db/v1/order_order.pb.orm.go",

		// Handler stubs
		"handlers/api/handlers_gen.go",
		"handlers/inventory/handlers_gen.go",

		// Bootstrap
		"pkg/app/bootstrap.go",
	}
	for _, rel := range mustExist {
		assertPathExistsE2E(t, filepath.Join(projectDir, rel))
	}

	// Add replace directive for forge/pkg (ORM imports forge/pkg/orm)
	addforgeReplace(t, filepath.Join(projectDir, "gen"))

	// go mod tidy on both module boundaries
	runCmd(t, projectDir, "go", "mod", "tidy")
	runCmd(t, filepath.Join(projectDir, "gen"), "go", "mod", "tidy")

	// ── Build — this is the primary assertion ───────────────────────────
	// If any generated code has invalid Go (wrong types for enums, broken
	// map/repeated handling, missing imports), this fails.
	runCmd(t, projectDir, "go", "build", "./...")

	// Vet — catches subtler issues like unreachable code or misuse of
	// printf verbs in generated templates.
	runCmd(t, projectDir, "go", "vet", "./...")

	// ── Content guards for complex types ────────────────────────────────

	// Verify the ORM code for Product includes the enum field and doesn't
	// fall back to "string" for it.
	productORM := readFileE2E(t, filepath.Join(projectDir, "gen", "db", "v1", "product_product.pb.orm.go"))
	if !strings.Contains(productORM, "forge/pkg/orm") {
		t.Error("product ORM should import forge/pkg/orm")
	}

	// Verify the API service handler stub references all 4 RPCs.
	apiHandlers := readFileE2E(t, filepath.Join(projectDir, "handlers", "api", "handlers_gen.go"))
	for _, rpc := range []string{"GetProduct", "ListProducts", "CreateProduct", "UpdateProduct"} {
		if !strings.Contains(apiHandlers, rpc) {
			t.Errorf("handlers_gen.go should contain handler for %s", rpc)
		}
	}

	// Verify inventory handler stub has all 3 RPCs.
	invHandlers := readFileE2E(t, filepath.Join(projectDir, "handlers", "inventory", "handlers_gen.go"))
	for _, rpc := range []string{"GetStockLevel", "AdjustStock", "ListAdjustments"} {
		if !strings.Contains(invHandlers, rpc) {
			t.Errorf("inventory handlers_gen.go should contain handler for %s", rpc)
		}
	}

	// Verify bootstrap includes both services.
	bootstrap := readFileE2E(t, filepath.Join(projectDir, "pkg", "app", "bootstrap.go"))
	if !strings.Contains(bootstrap, "api.New(") {
		t.Error("bootstrap should register api service")
	}
	if !strings.Contains(bootstrap, "inventory.New(") {
		t.Error("bootstrap should register inventory service")
	}

	// Verify generated proto Go code for enums compiles (checked by
	// go build above, but spot-check the file content).
	apiPB := readFileE2E(t, filepath.Join(projectDir, "gen", "services", "api", "v1", "api.pb.go"))
	if !strings.Contains(apiPB, "SortOrder") {
		t.Error("api.pb.go should contain SortOrder enum type")
	}

	inventoryPB := readFileE2E(t, filepath.Join(projectDir, "gen", "services", "inventory", "v1", "inventory.pb.go"))
	if !strings.Contains(inventoryPB, "AdjustmentType") {
		t.Error("inventory.pb.go should contain AdjustmentType enum type")
	}

	productPB := readFileE2E(t, filepath.Join(projectDir, "gen", "db", "v1", "product.pb.go"))
	if !strings.Contains(productPB, "ProductStatus") {
		t.Error("product.pb.go should contain ProductStatus enum type")
	}
	if !strings.Contains(productPB, "Dimension") {
		t.Error("product.pb.go should contain Dimension nested message type")
	}

	orderPB := readFileE2E(t, filepath.Join(projectDir, "gen", "db", "v1", "order.pb.go"))
	if !strings.Contains(orderPB, "OrderStatus") {
		t.Error("order.pb.go should contain OrderStatus enum type")
	}
	if !strings.Contains(orderPB, "LineItem") {
		t.Error("order.pb.go should contain LineItem nested message type")
	}
	if !strings.Contains(orderPB, "Address") {
		t.Error("order.pb.go should contain Address nested message type")
	}
}

// TestE2EScaffoldEntityInServiceProto verifies that entity annotations on
// messages inside proto/services/ (not proto/db/) are properly detected by
// the descriptor extractor. This is a regression test for the bug where
// looksLikeEntity() only checked for "db/" in the file path.
func TestE2EScaffoldEntityInServiceProto(t *testing.T) {
	forgeBin := buildforgeBinary(t)
	dir := t.TempDir()

	runCmd(t, dir, forgeBin,
		"new", "svcentity",
		"--mod", "example.com/svcentity",
		"--service", "api",
	)

	projectDir := filepath.Join(dir, "svcentity")

	// Write an entity proto that lives in proto/services/ instead of
	// proto/db/. The entity has explicit entity_options, so
	// looksLikeEntity() shouldn't need to rely on path heuristics.
	svcProtoDir := filepath.Join(projectDir, "proto", "services", "api", "v1")
	svcProto := `syntax = "proto3";

package services.api.v1;

import "forge/options/v1/entity.proto";
import "forge/options/v1/field.proto";
import "forge/options/v1/service.proto";
import "google/protobuf/timestamp.proto";

option go_package = "example.com/svcentity/gen/services/api/v1;apiv1";

// Tenant is a DB entity defined alongside the service proto.
// This pattern occurs when plan protos at proto/.plan/ generate
// both the API service and the entity in the same package.
message Tenant {
  option (forge.options.v1.entity_options) = {
    table_name: "tenants"
    timestamps: true
  };

  string id = 1 [(forge.options.v1.field_options) = {
    primary_key: true
    not_null: true
  }];

  string name = 2 [(forge.options.v1.field_options) = {
    not_null: true
  }];

  string slug = 3 [(forge.options.v1.field_options) = {
    not_null: true
  }];

  google.protobuf.Timestamp created_at = 4 [(forge.options.v1.field_options) = {
    not_null: true
    default_value: "NOW()"
  }];

  google.protobuf.Timestamp updated_at = 5 [(forge.options.v1.field_options) = {
    not_null: true
    default_value: "NOW()"
  }];
}

service ApiService {
  option (forge.options.v1.service_options) = {
    name: "ApiService"
    version: "1.0.0"
    description: "API with co-located entity"
  };

  rpc GetTenant(GetTenantRequest) returns (GetTenantResponse) {}
  rpc ListTenants(ListTenantsRequest) returns (ListTenantsResponse) {}
}

message GetTenantRequest {
  string id = 1;
}

message GetTenantResponse {
  Tenant tenant = 1;
}

message ListTenantsRequest {
  int32 page_size = 1;
  string page_token = 2;
}

message ListTenantsResponse {
  repeated Tenant tenants = 1;
  string next_page_token = 2;
}
`
	if err := os.WriteFile(filepath.Join(svcProtoDir, "api.proto"), []byte(svcProto), 0o644); err != nil {
		t.Fatalf("write api.proto: %v", err)
	}

	// Generate
	runCmd(t, projectDir, forgeBin, "generate")

	// The entity should have ORM code generated even though it's in
	// proto/services/ rather than proto/db/.
	// Note: the ORM output path may vary — check both possible locations.
	ormInServices := filepath.Join(projectDir, "gen", "services", "api", "v1", "api_tenant.pb.orm.go")
	ormInDB := filepath.Join(projectDir, "gen", "db", "v1", "tenant.pb.orm.go")

	servicesOrmExists := fileExists(ormInServices)
	dbOrmExists := fileExists(ormInDB)

	if !servicesOrmExists && !dbOrmExists {
		t.Fatalf("expected ORM code for Tenant entity at either %s or %s — neither exists.\n"+
			"This indicates looksLikeEntity() is not detecting entities with explicit entity_options outside proto/db/.",
			ormInServices, ormInDB)
	}

	// Add forge replace for ORM imports
	addforgeReplace(t, filepath.Join(projectDir, "gen"))

	// go mod tidy
	runCmd(t, projectDir, "go", "mod", "tidy")
	runCmd(t, filepath.Join(projectDir, "gen"), "go", "mod", "tidy")

	// Build — the entity must produce valid Go.
	runCmd(t, projectDir, "go", "build", "./...")

	// Handler stub should exist and contain RPCs.
	handlers := readFileE2E(t, filepath.Join(projectDir, "handlers", "api", "handlers_gen.go"))
	if !strings.Contains(handlers, "GetTenant") {
		t.Error("handlers_gen.go should contain GetTenant handler")
	}
	if !strings.Contains(handlers, "ListTenants") {
		t.Error("handlers_gen.go should contain ListTenants handler")
	}
}

// TestE2EScaffoldConfigNaming verifies that the generated config struct
// field names match what the templates reference. This is a regression test
// for the DatabaseURL vs DatabaseUrl naming mismatch.
func TestE2EScaffoldConfigNaming(t *testing.T) {
	forgeBin := buildforgeBinary(t)
	dir := t.TempDir()

	runCmd(t, dir, forgeBin,
		"new", "cfgtest",
		"--mod", "example.com/cfgtest",
		"--service", "api",
	)

	projectDir := filepath.Join(dir, "cfgtest")

	// Generate code (produces pkg/config/config.go from config.proto)
	runCmd(t, projectDir, forgeBin, "generate")

	// Read the generated config and all templates that reference config fields.
	configGo := readFileE2E(t, filepath.Join(projectDir, "pkg", "config", "config.go"))

	// Find all config field names referenced in cmd/ files.
	cmdFiles := []string{"cmd/server.go", "cmd/db.go"}
	for _, rel := range cmdFiles {
		cmdPath := filepath.Join(projectDir, rel)
		if !fileExists(cmdPath) {
			continue
		}
		cmdContent := readFileE2E(t, cmdPath)

		// Extract cfg.Xxx references from template-generated code.
		// If the template uses cfg.DatabaseURL but config.go defines
		// cfg.DatabaseUrl, the build will fail — but this test catches
		// the mismatch with a clear error message before build.
		for _, field := range extractConfigFieldRefs(cmdContent) {
			if !strings.Contains(configGo, field) {
				t.Errorf("%s references cfg.%s but pkg/config/config.go does not define it.\n"+
					"This is likely a naming mismatch between the template and the config generator.\n"+
					"Config struct fields:\n%s",
					rel, field, extractStructFields(configGo, "Config"))
			}
		}
	}

	// Also verify the setup.go file if it exists.
	setupPath := filepath.Join(projectDir, "pkg", "app", "setup.go")
	if fileExists(setupPath) {
		setupContent := readFileE2E(t, setupPath)
		for _, field := range extractConfigFieldRefs(setupContent) {
			if !strings.Contains(configGo, field) {
				t.Errorf("pkg/app/setup.go references cfg.%s but pkg/config/config.go does not define it",
					field)
			}
		}
	}

	// go mod tidy + build as a final assertion.
	runCmd(t, projectDir, "go", "mod", "tidy")
	runCmd(t, filepath.Join(projectDir, "gen"), "go", "mod", "tidy")
	runCmd(t, projectDir, "go", "build", "./...")
}

// TestE2EScaffoldNoConflictingProtos verifies that `forge new` does not
// scaffold both proto/forge/v1/forge.proto AND proto/forge/options/v1/*.proto,
// which would produce conflicting extension tag numbers and break buf generate.
func TestE2EScaffoldNoConflictingProtos(t *testing.T) {
	forgeBin := buildforgeBinary(t)
	dir := t.TempDir()

	runCmd(t, dir, forgeBin,
		"new", "protocheck",
		"--mod", "example.com/protocheck",
		"--service", "api",
	)

	projectDir := filepath.Join(dir, "protocheck")

	// Check which proto annotation format was scaffolded.
	hasOptionsV1 := fileExists(filepath.Join(projectDir, "proto", "forge", "options", "v1", "entity.proto"))
	hasForgeV1 := fileExists(filepath.Join(projectDir, "proto", "forge", "v1", "forge.proto"))

	if hasOptionsV1 && hasForgeV1 {
		t.Fatal("scaffold created BOTH proto/forge/options/v1/*.proto AND proto/forge/v1/forge.proto — " +
			"these have conflicting extension tag numbers and will break buf generate. " +
			"Only one annotation format should be scaffolded.")
	}

	if !hasOptionsV1 && !hasForgeV1 {
		t.Fatal("scaffold created neither proto/forge/options/v1/ nor proto/forge/v1/ — " +
			"at least one annotation format must be scaffolded for forge generate to work")
	}

	// Whichever format was scaffolded, verify generate + build works.
	runCmd(t, projectDir, forgeBin, "generate")
	runCmd(t, projectDir, "go", "mod", "tidy")
	runCmd(t, filepath.Join(projectDir, "gen"), "go", "mod", "tidy")
	runCmd(t, projectDir, "go", "build", "./...")
}

// ── Helpers ────────────────────────────────────────────────────────────────

// extractConfigFieldRefs finds all `cfg.XxxYyy` references in Go source
// and returns the field names (e.g. "DatabaseURL", "Port").
func extractConfigFieldRefs(content string) []string {
	var fields []string
	seen := make(map[string]bool)

	// Simple scanner: find "cfg." followed by an uppercase identifier.
	for i := 0; i < len(content)-4; i++ {
		if content[i:i+4] != "cfg." {
			continue
		}
		j := i + 4
		if j >= len(content) || content[j] < 'A' || content[j] > 'Z' {
			continue
		}
		end := j
		for end < len(content) && (content[end] >= 'a' && content[end] <= 'z' ||
			content[end] >= 'A' && content[end] <= 'Z' ||
			content[end] >= '0' && content[end] <= '9' ||
			content[end] == '_') {
			end++
		}
		field := content[j:end]
		if !seen[field] {
			seen[field] = true
			fields = append(fields, field)
		}
	}
	return fields
}

// extractStructFields extracts the field declaration block from a Go struct
// for diagnostic output in test failure messages.
func extractStructFields(content string, structName string) string {
	marker := "type " + structName + " struct {"
	idx := strings.Index(content, marker)
	if idx < 0 {
		return "(struct not found)"
	}
	// Find the closing brace.
	depth := 0
	start := idx
	for i := idx; i < len(content); i++ {
		if content[i] == '{' {
			depth++
		} else if content[i] == '}' {
			depth--
			if depth == 0 {
				return content[start : i+1]
			}
		}
	}
	// Truncate if we can't find the end.
	end := start + 500
	if end > len(content) {
		end = len(content)
	}
	return content[start:end] + "…"
}