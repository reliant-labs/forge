package mcpkit

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"testing"

	"buf.build/gen/go/bufbuild/protovalidate/protocolbuffers/go/buf/validate"
	"google.golang.org/protobuf/proto"
)

// An InvalidArgument carrying protovalidate's violations names each field,
// so a model can correct its arguments.
func TestConnectErrorViolations(t *testing.T) {
	vs := &validate.Violations{Violations: []*validate.Violation{{
		Field:   &validate.FieldPath{Elements: []*validate.FieldPathElement{{FieldName: proto.String("name")}}},
		Message: proto.String("value is required"),
	}}}
	raw, _ := proto.Marshal(vs)
	body, _ := json.Marshal(map[string]any{
		"code": "invalid_argument", "message": "validation failed",
		"details": []map[string]string{{"type": "buf.validate.Violations", "value": base64.RawStdEncoding.EncodeToString(raw)}},
	})
	ce := decodeConnectError(400, body)
	got := ce.violations()
	if ce.Code != "invalid_argument" || len(got) != 1 || got[0] != "name: value is required" {
		t.Fatalf("%+v %v", ce, got)
	}
	if r := errorResult(ce.Code, ce.Message, got); !r.IsError {
		t.Fatal("not an error result")
	}
	if ce := decodeConnectError(503, []byte("down")); ce.Code != "unavailable" || ce.Message != "down" {
		t.Fatalf("bare status: %+v", ce)
	}
}

func TestLimiter(t *testing.T) {
	l := newLimiter(-1, 0, 2)
	r1, ok1 := l.acquire("k")
	_, ok2 := l.acquire("k")
	if _, ok3 := l.acquire("k"); !ok1 || !ok2 || ok3 {
		t.Fatal("in-flight bound not applied")
	}
	if _, ok := l.acquire("other"); !ok {
		t.Fatal("keys share a bound")
	}
	r1()
	if _, ok := l.acquire("k"); !ok {
		t.Fatal("release did not free a slot")
	}
	b := newLimiter(60, 2, -1)
	_, a := b.acquire("k")
	_, c := b.acquire("k")
	if _, d := b.acquire("k"); !a || !c || d {
		t.Fatal("burst not applied")
	}
}

func TestNewRefusesBadTables(t *testing.T) {
	up := nopHandler{}
	if _, err := New(Config{Tools: []Tool{{Name: "a", Procedure: "/p"}, {Name: "a", Procedure: "/q"}}}, up); err == nil {
		t.Fatal("duplicate accepted")
	}
	if _, err := New(Config{Tools: []Tool{{Name: "a"}}}, up); err == nil {
		t.Fatal("empty procedure accepted")
	}
}

type nopHandler struct{}

func (nopHandler) ServeHTTP(http.ResponseWriter, *http.Request) {}
