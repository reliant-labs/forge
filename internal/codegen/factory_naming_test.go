package codegen

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// CP's service.go:250 declares a NewDeployment INPUT TYPE, not a compatible
// test factory. An incompatible function constructor is equally valid owned
// code. Generation must keep both APIs, never substitute one for the other.
func TestEntityFactories_CPOccupiedNameCompiles(t *testing.T) {
	for _, owned := range []string{
		"package orders\ntype NewDeployment struct { OrgID, EnvironmentID, Name string }\n",
		"package orders\ntype DeploymentInput struct { Name string }; func NewDeployment(in DeploymentInput) *DeploymentInput { return &in }\n",
		"package orders\ntype NewDeployment struct{}; func ForgeNewDeployment() {}; func ForgeNewDeployment2() {}\n",
	} {
		project, dir := writeHandlerFiles(t, map[string]string{"service.go": owned})
		stubFactorySchema(t, project)
		svc := ServiceDef{Name: "OrdersService", Methods: []Method{{Name: "GetDeployment"}, {Name: "ListDeployments"}}}
		for i := 0; i < 2; i++ {
			if err := GenerateEntityFactories(project, "example.com/test", []ServiceDef{svc}, nil); err != nil {
				t.Fatalf("valid pre-existing CP declarations must not block generation: %v", err)
			}
			out, err := os.ReadFile(filepath.Join(dir, entityFactoryFile))
			if err != nil {
				t.Fatal(err)
			}
			name := "ForgeNewDeployment"
			if strings.Contains(owned, "func ForgeNewDeployment2") {
				name += "3"
			}
			if !strings.Contains(string(out), "func "+name+"(") {
				t.Fatalf("missing collision-free factory %s", name)
			}
			// Compile generated SQL execution, ORM calls and exported signature together
			// with the real owned declaration shape and a caller using the chosen name.
			checkFactoryTypes(t, out, owned, "package orders; import \"testing\"; import \"github.com/reliant-labs/forge/pkg/orm\"; func useFactory(t testing.TB, database orm.Context) { _ = "+name+"(t,database) }")
		}
	}
}

func TestEntityFactories_RequestCallerUsesChosenName(t *testing.T) {
	mine := "package orders\ntype NewCreateWidgetRequest struct{}; func ForgeNewCreateWidgetRequest() {}\n"
	project, dir := writeHandlerFiles(t, map[string]string{"service.go": mine})
	stubFactorySchema(t, project)
	entity := EntityDef{Name: "Widget", TableName: "widgets", PkField: "id", PkGoType: "string", Fields: []EntityField{{Name: "id", GoName: "Id", ProtoType: "string", GoType: "string", Kind: FieldKindScalar}}}
	svc := ServiceDef{Name: "OrdersService", GoPackage: "example.com/test/pb", Messages: map[string][]MessageFieldDef{"Widget": {{Name: "id", ProtoType: "string"}}, "CreateWidgetRequest": {}}}
	methods := []CRUDMethod{
		{Method: MethodTemplateData{Name: "CreateWidget", InputType: "CreateWidgetRequest", OutputType: "CreateWidgetResponse"}, Entity: entity, Operation: "create"},
		{Method: MethodTemplateData{Name: "GetWidget", InputType: "GetWidgetRequest", OutputType: "GetWidgetResponse"}, Entity: entity, Operation: "get"},
	}
	for _, cm := range methods {
		svc.Methods = append(svc.Methods, Method{Name: cm.Method.Name, InputType: cm.Method.InputType, OutputType: cm.Method.OutputType})
	}
	if _, err := GenerateCRUDTests(svc, methods, "example.com/test", project, nil); err != nil {
		t.Fatal(err)
	}
	caller, err := os.ReadFile(filepath.Join(dir, "handlers_crud_test.go"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(caller), "orders.ForgeNewCreateWidgetRequest2(t, db, 0") || !strings.Contains(string(caller), "orders.ForgeNewCreateWidgetRequest2(t, db, 1") {
		t.Fatalf("scaffolded caller did not use chosen factory:\n%s", caller)
	}
	if err := GenerateEntityFactories(project, "example.com/test", []ServiceDef{svc}, nil); err != nil {
		t.Fatal(err)
	}
	out, err := os.ReadFile(filepath.Join(dir, entityFactoryFile))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(out), "func ForgeNewCreateWidgetRequest2(") {
		t.Fatal("emitter and caller chose different names")
	}
	checkFactoryTypes(t, out, mine, "package orders; import \"testing\"; import \"github.com/reliant-labs/forge/pkg/orm\"; func useFactory(t testing.TB, database orm.Context) { _ = ForgeNewCreateWidgetRequest2(t,database,0) }")
}
