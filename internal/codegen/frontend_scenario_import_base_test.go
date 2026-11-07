package codegen

import "testing"

func TestScenarioRPCWorkspaceImportBase(t *testing.T) {
	services := []ServiceDef{{Name: "WidgetService", Package: "services.widget.v1", ProtoFile: "services/widget/v1/widget.proto", Methods: []Method{{Name: "GetWidget", InputType: "GetWidgetRequest", OutputType: "GetWidgetResponse"}}}}
	for _, base := range []string{"@/gen", "@fixture/api"} {
		data := BuildScenarioRPCDataWithImportBase(services, base)
		entry := data.Services[0].Entries[0]
		if entry.RequestType != `import("`+base+`/services/widget/v1/widget_pb").GetWidgetRequest` {
			t.Fatalf("request import %s", entry.RequestType)
		}
		if entry.ResponseSchema != `import("`+base+`/services/widget/v1/widget_pb").GetWidgetResponseSchema` {
			t.Fatalf("response import %s", entry.ResponseSchema)
		}
	}
	legacy := BuildScenarioRPCData(services)
	if legacy.Services[0].Entries[0].RequestType != BuildScenarioRPCDataWithImportBase(services, "@/gen").Services[0].Entries[0].RequestType {
		t.Fatal("legacy import contract changed")
	}
}
