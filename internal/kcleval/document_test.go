package kcleval_test

// document_test.go pins the parts of kcleval that need no KCL evaluation:
// decoding the render's JSON, selecting from it, encoding it back, and the
// refusals Eval makes before it evaluates anything. They run under -short.

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/reliant-labs/forge/internal/kcleval"
)

func mustDecode(t *testing.T, raw string) any {
	t.Helper()
	doc, err := kcleval.Decode([]byte(raw))
	if err != nil {
		t.Fatalf("Decode(%q): %v", raw, err)
	}
	return doc
}

func TestDecodeRejectsMalformedDocuments(t *testing.T) {
	for name, raw := range map[string]string{
		"empty":                 ``,
		"truncated object":      `{"a":`,
		"truncated list":        `[1,2`,
		"unterminated string":   `{"a":"b`,
		"trailing content":      `{"a":1} {"b":2}`,
		"trailing garbage":      `{"a":1} x`,
		"bad member in object":  `{"a":1,}`,
		"bad element in a list": `[1,]`,
		"value then key":        `{"a" 1}`,
	} {
		t.Run(name, func(t *testing.T) {
			if got, err := kcleval.Decode([]byte(raw)); err == nil {
				t.Fatalf("Decode(%q) = %#v, want an error", raw, got)
			}
		})
	}
}

func TestDecodeTrailingContentNamesTheProblem(t *testing.T) {
	_, err := kcleval.Decode([]byte(`{"a":1} {"b":2}`))
	if err == nil || !strings.Contains(err.Error(), "after the top-level value") {
		t.Fatalf("err = %v, want it to say content follows the top-level value", err)
	}
}

// Numbers keep KCL's own text, and members keep declared (not alphabetical)
// order, through a decode and an encode.
func TestDecodeKeepsOrderAndNumberText(t *testing.T) {
	const raw = `{"zeta":1.0,"alpha":1e+20,"mid":[true,null,"s",{"k":2}]}`
	doc := mustDecode(t, raw)
	obj, ok := doc.(kcleval.Object)
	if !ok || len(obj) != 3 {
		t.Fatalf("Decode = %#v, want a 3-member Object", doc)
	}
	if keys := []string{obj[0].Key, obj[1].Key, obj[2].Key}; strings.Join(keys, ",") != "zeta,alpha,mid" {
		t.Errorf("member order = %v, want declared order zeta,alpha,mid", keys)
	}
	if obj[0].Value != json.Number("1.0") || obj[1].Value != json.Number("1e+20") {
		t.Errorf("numbers = %#v, %#v; want KCL's own text", obj[0].Value, obj[1].Value)
	}
	out, err := kcleval.Render(kcleval.Result{Value: doc}, kcleval.FormatJSON)
	if err != nil {
		t.Fatal(err)
	}
	const want = `{
    "zeta": 1.0,
    "alpha": 1e+20,
    "mid": [
        true,
        null,
        "s",
        {
            "k": 2
        }
    ]
}
`
	if string(out) != want {
		t.Errorf("JSON =\n%s\nwant\n%s", out, want)
	}
}

func TestRenderJSONEmptyContainersAndScalars(t *testing.T) {
	for name, tc := range map[string]struct {
		val  any
		want string
	}{
		"empty object": {kcleval.Object{}, "{}\n"},
		"empty list":   {[]any{}, "[]\n"},
		"null":         {nil, "null\n"},
		"false":        {false, "false\n"},
		"number":       {json.Number("200"), "200\n"},
	} {
		t.Run(name, func(t *testing.T) {
			out, err := kcleval.Render(kcleval.Result{Value: tc.val}, kcleval.FormatJSON)
			if err != nil || string(out) != tc.want {
				t.Fatalf("Render = %q, %v; want %q", out, err, tc.want)
			}
		})
	}
}

// Strings are escaped with only what JSON requires, matching `kcl run`:
// markup characters stay literal, control characters are escaped.
func TestRenderJSONStringEscaping(t *testing.T) {
	for name, tc := range map[string]struct{ in, want string }{
		"quote and backslash": {`a"b\c`, `"a\"b\\c"`},
		"whitespace escapes":  {"l1\nl2\r\t", `"l1\nl2\r\t"`},
		"backspace formfeed":  {"\b\f", `"\b\f"`},
		"other control":       {"\x01\x1f", `"\u0001\u001f"`},
		"html stays literal":  {"<a&b>", `"<a&b>"`},
		"unicode stays":       {"héllo ☃ \u2028", "\"héllo ☃ \u2028\""},
	} {
		t.Run(name, func(t *testing.T) {
			out, err := kcleval.Render(kcleval.Result{Value: tc.in, Scalar: true}, kcleval.FormatJSON)
			if err != nil {
				t.Fatal(err)
			}
			if got := strings.TrimSuffix(string(out), "\n"); got != tc.want {
				t.Errorf("encoded %q as %s, want %s", tc.in, got, tc.want)
			}
		})
	}
}

func TestRenderYAML(t *testing.T) {
	doc := mustDecode(t, `{"b":"0755","a":{"y":"yes","n":"x"},"list":[1,2.5,true,null],"empty":[],"on":"On","o":{}}`)
	out, err := kcleval.Render(kcleval.Result{Value: doc}, kcleval.FormatYAML)
	if err != nil {
		t.Fatal(err)
	}
	// Declared order is kept; "0755" is quoted so it stays a string; the
	// YAML 1.1 booleans y/n/yes/on are quoted so a 1.1 reader keeps strings.
	const want = `b: "0755"
a:
  'y': 'yes'
  'n': x
list:
- 1
- 2.5
- true
- null
empty: []
'on': 'On'
o: {}
`
	if string(out) != want {
		t.Errorf("YAML =\n%s\nwant\n%s", out, want)
	}
}

// A value that is not part of a decoded document cannot be encoded: the
// encoders name the type instead of printing something wrong.
func TestRenderRefusesAnUndecodedType(t *testing.T) {
	for _, f := range []kcleval.Format{kcleval.FormatJSON, kcleval.FormatYAML} {
		for name, val := range map[string]any{
			"float64":     1.5,
			"nested map":  kcleval.Object{{Key: "k", Value: map[string]any{"a": 1}}},
			"nested list": []any{struct{}{}},
		} {
			_, err := kcleval.Render(kcleval.Result{Value: val}, f)
			if err == nil || !strings.Contains(err.Error(), "cannot encode") {
				t.Errorf("%s as %s: err = %v, want a \"cannot encode\" refusal", name, f, err)
			}
		}
	}
}

func TestSelectRefusals(t *testing.T) {
	doc := mustDecode(t, `{"pool":{"name":"a","taint":null},"list":["x","y"],"n":3,"s":"str","b":true,"f":null}`)
	for _, tc := range []struct {
		name, selector string
		wantErr        []string
	}{
		{"typo names what exists", "pool.taint_ky", []string{"pool has no field", `"taint_ky"`, "name, taint"}},
		{"missing top-level field lists the document's fields", "nope", []string{"the document has no field", "b, f, list, n, pool, s"}},
		{"list needs an index", "list.first", []string{"list of 2", `"first" must be an index`}},
		{"index out of range", "list.5", []string{"index 5 is out of range"}},
		{"negative index", "list.-1", []string{"index -1 is out of range"}},
		{"field of a number", "n.x", []string{"a number", `no field "x"`}},
		{"field of a string", "s.x", []string{"a string"}},
		{"field of a bool", "b.x", []string{"a bool"}},
		{"field of None", "f.x", []string{"None"}},
		{"empty segment", "pool..name", []string{"empty segment"}},
		{"leading dot", ".pool", []string{"empty segment"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := kcleval.Select(doc, tc.selector)
			if err == nil {
				t.Fatalf("Select(%q) = %#v, want an error", tc.selector, got)
			}
			for _, w := range tc.wantErr {
				if !strings.Contains(err.Error(), w) {
					t.Errorf("error %q lacks %q", err, w)
				}
			}
		})
	}
	// Absent-vs-broken is the reason ErrNoSuchField exists.
	for _, sel := range []string{"nope", "list.first", "list.5", "n.x"} {
		if _, err := kcleval.Select(doc, sel); !errors.Is(err, kcleval.ErrNoSuchField) {
			t.Errorf("Select(%q) error %v is not ErrNoSuchField", sel, err)
		}
	}
	if _, err := kcleval.Select(doc, "pool..name"); errors.Is(err, kcleval.ErrNoSuchField) {
		t.Error("a malformed selector must not read as an absent field")
	}
}

func TestSelectOfAnObjectWithNoFields(t *testing.T) {
	_, err := kcleval.Select(kcleval.Object{}, "x")
	if err == nil || !strings.Contains(err.Error(), "no fields") {
		t.Fatalf("err = %v, want it to say the object has no fields", err)
	}
}

// A document with many keys lists the first dozen and counts the rest, so the
// refusal does not bury itself.
func TestSelectCapsTheFieldList(t *testing.T) {
	var obj kcleval.Object
	for i := range 20 {
		obj = append(obj, kcleval.Member{Key: fmt.Sprintf("k%02d", i), Value: json.Number("1")})
	}
	_, err := kcleval.Select(obj, "missing")
	if err == nil {
		t.Fatal("want an error")
	}
	for _, want := range []string{"k00", "k11", "(8 more)"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q lacks %q", err, want)
		}
	}
	if strings.Contains(err.Error(), "k12") {
		t.Errorf("error %q lists past the cap", err)
	}
}

// Eval refuses a request before it evaluates anything. None of these reach
// KCL, so they cost nothing.
func TestEvalRefusesBadRequests(t *testing.T) {
	root := t.TempDir()
	write := func(rel, body string) {
		t.Helper()
		p := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("notes.txt", "x")
	outside := filepath.Join(t.TempDir(), "elsewhere.k")
	if err := os.WriteFile(outside, []byte("a = 1\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct {
		name string
		req  kcleval.Request
		want string
	}{
		{"no file", kcleval.Request{ProjectDir: root}, "no file given"},
		{"missing file", kcleval.Request{ProjectDir: root, File: "absent.k"}, "read absent.k"},
		{"not a .k file", kcleval.Request{ProjectDir: root, File: "notes.txt"}, "is not a .k file"},
		{"relative escape", kcleval.Request{ProjectDir: root, File: filepath.Join("..", "x.k")}, "outside the project"},
		{"absolute path elsewhere", kcleval.Request{ProjectDir: root, File: outside}, "outside the project"},
		{"process rooted but cwd is elsewhere", kcleval.Request{ProjectDir: root, File: "notes.txt", ProcessRooted: true}, "is not a .k file"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			res, err := kcleval.Eval(tc.req)
			if err == nil {
				t.Fatalf("Eval = %+v, want an error", res)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error %q lacks %q", err, tc.want)
			}
		})
	}
}

// ProcessRooted promises the caller already stands at the project root; a
// caller that does not is told, rather than evaluated against the wrong cwd.
func TestEvalProcessRootedRefusesAWrongWorkingDirectory(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "main.k"), []byte("a = 1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := kcleval.Eval(kcleval.Request{ProjectDir: root, File: "main.k", ProcessRooted: true})
	if err == nil || !strings.Contains(err.Error(), "is not project root") {
		t.Fatalf("err = %v, want the cwd-is-not-project-root refusal", err)
	}
}

func TestFormats(t *testing.T) {
	if got := strings.Join(kcleval.Formats(), ","); got != "json,yaml,raw" {
		t.Errorf("Formats() = %s, want json,yaml,raw", got)
	}
}
