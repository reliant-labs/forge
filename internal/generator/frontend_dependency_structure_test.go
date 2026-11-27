package generator

import (
	"encoding/json"
	"testing"
)

func TestInsertDependencyStructuralMembers(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		before string
		want   string
	}{
		{
			name:   "nested value before later member",
			before: `{"dependencies":{"alpha":{"zulu":"nested","list":[{"x":true}]},"beta":"1","zulu":"2"}}`,
			want:   `{"dependencies":{"alpha":{"zulu":"nested","list":[{"x":true}]},"beta":"1","middle": "3", "zulu":"2"}}`,
		},
		{
			name:   "append after complete nested value",
			before: `{"dependencies":{"alpha":{"zulu":"nested","list":[1,null,false]}}}`,
			want:   `{"dependencies":{"alpha":{"zulu":"nested","list":[1,null,false]}, "middle": "3"}}`,
		},
		{
			name:   "braces and escaped quotes in strings",
			before: `{"dependencies":{"alpha":"} \\ { \"quoted\"","beta":"1","zulu":"2"}}`,
			want:   `{"dependencies":{"alpha":"} \\ { \"quoted\"","beta":"1","middle": "3", "zulu":"2"}}`,
		},
		{
			name:   "ignore nested dependencies object",
			before: `{"config":{"dependencies":{"zulu":"untouched"}},"dependencies":{"alpha":"1","zulu":"2"}}`,
			want:   `{"config":{"dependencies":{"zulu":"untouched"}},"dependencies":{"alpha":"1","middle": "3", "zulu":"2"}}`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if !json.Valid([]byte(tc.before)) {
				t.Fatal("invalid test fixture")
			}
			got, err := insertDependency(tc.before, "middle", "3")
			if err != nil {
				t.Fatal(err)
			}
			if got != tc.want {
				t.Fatalf("insertDependency = %s; want %s", got, tc.want)
			}
			if !json.Valid([]byte(got)) {
				t.Fatalf("produced invalid JSON: %s", got)
			}
		})
	}
}
