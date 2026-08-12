package cmdutil

import (
	"strings"
	"testing"
)

func TestValidateServiceDirConsistency(t *testing.T) {
	tests := []struct {
		name             string
		protoServiceName string
		dirName          string
		wantErr          bool
		wantContainsAll  []string
	}{
		{
			name:             "matching name and directory passes",
			protoServiceName: "WorkorderService",
			dirName:          "workorder",
			wantErr:          false,
		},
		{
			name:             "multiword directory with matching pascal service passes",
			protoServiceName: "WorkOrderService",
			dirName:          "work_order",
			wantErr:          false,
		},
		{
			name:             "camel-cased proto service in single-word directory fails with both fixes named",
			protoServiceName: "WorkOrderService",
			dirName:          "workorder",
			wantErr:          true,
			wantContainsAll: []string{
				`proto service "WorkOrderService" in directory "workorder"`,
				"NewWorkOrderServiceHandler",
				"MountWorkorder",
				`rename the proto service to "WorkorderService"`,
				`rename the directory to "work_order"`,
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := ValidateServiceDirConsistency(tt.protoServiceName, tt.dirName)
			if tt.wantErr && err == nil {
				t.Fatalf("expected an error, got nil")
			}
			if !tt.wantErr && err != nil {
				t.Fatalf("expected no error, got: %v", err)
			}
			for _, want := range tt.wantContainsAll {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("error message missing %q\ngot: %s", want, err.Error())
				}
			}
		})
	}
}
