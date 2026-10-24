package obsquery

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"strconv"
	"strings"
	"testing"

	semconv "go.opentelemetry.io/otel/semconv/v1.43.0"

	"github.com/reliant-labs/forge/pkg/observe"
	"github.com/reliant-labs/forge/pkg/svcerr"
)

// rec builds an ERROR record logged at fn.
func rec(service, fn, body string, attrs ...string) ServerErrorRecord {
	a := map[string]string{LogKeyCodeFunction: fn, LogKeyCodeFile: "internal/x/x.go", "code.line.number": "42"}
	for i := 0; i+1 < len(attrs); i += 2 {
		a[attrs[i]] = attrs[i+1]
	}
	return ServerErrorRecord{ServiceName: service, ScopeName: "github.com/acme/app", Body: body, Attributes: a}
}

// The keys sfp1 reads are the keys forge's SDK writes.
func TestFingerprintKeysAreTheSDKs(t *testing.T) {
	for got, want := range map[string]string{
		LogKeyCodeFunction:     string(semconv.CodeFunctionNameKey),
		LogKeyCodeFile:         string(semconv.CodeFilePathKey),
		LogKeyExceptionType:    observe.LogKeyExceptionType,
		LogKeyExceptionMessage: observe.LogKeyExceptionMessage,
		LogKeyErrorGroup:       observe.LogKeyErrorGroup,
		LogKeyIssueSource:      observe.LogKeyIssueSource,
	} {
		if got != want {
			t.Errorf("key %q, SDK writes %q", got, want)
		}
	}
}

func TestServerFingerprintGrouping(t *testing.T) {
	const fn = "github.com/acme/app/internal/handlers/jobs.(*Service).ListJobs"
	pg := []string{LogKeyExceptionType, "*pgconn.PgError", LogKeyExceptionMessage, "ERROR: relation \"jobs\" does not exist (SQLSTATE 42P01)"}
	base := rec("api", fn, "list jobs failed", pg...)

	same := map[string]ServerErrorRecord{
		"another line number": func() ServerErrorRecord {
			r := rec("api", fn, "list jobs failed", pg...)
			r.Attributes["code.line.number"] = "99"
			return r
		}(),
		"another file path": func() ServerErrorRecord {
			r := rec("api", fn, "list jobs failed", pg...)
			r.Attributes[LogKeyCodeFile] = "moved/x.go"
			return r
		}(),
		"a non-generic error's message differs": rec("api", fn, "list jobs failed",
			LogKeyExceptionType, "*pgconn.PgError", LogKeyExceptionMessage, "something else entirely"),
	}
	for name, r := range same {
		if ServerFingerprint(r) != ServerFingerprint(base) {
			t.Errorf("%s: split into a second issue", name)
		}
	}

	// The same call site with ids that change on every event is one issue.
	a := rec("api", fn, "job 1234 for crew 7f3a9c21-0000-4000-8000-000000000001 failed at https://x.example/a?b=1", pg...)
	b := rec("api", fn, "job 98 for crew 11111111-2222-4333-8444-555555555555 failed at https://y.example/z", pg...)
	if ServerFingerprint(a) != ServerFingerprint(b) {
		t.Errorf("changing ids split one call site into two issues:\n%q\n%q", NormalizeMessage(a.Body), NormalizeMessage(b.Body))
	}

	different := map[string]ServerErrorRecord{
		"another service":  rec("worker", fn, "list jobs failed", pg...),
		"another function": rec("api", fn+"2", "list jobs failed", pg...),
		"another message":  rec("api", fn, "count jobs failed", pg...),
		"another type": rec("api", fn, "list jobs failed",
			LogKeyExceptionType, "context.deadlineExceededError"),
		"rpc failed in another procedure": rec("api", fn, "list jobs failed",
			append([]string{LogKeyProcedure, "/jobs.v1.JobService/ListJobs"}, pg...)...),
	}
	seen := map[string]string{ServerFingerprint(base): "base"}
	for name, r := range different {
		fp := ServerFingerprint(r)
		if prev, dup := seen[fp]; dup {
			t.Errorf("%s: same fingerprint as %s", name, prev)
		}
		seen[fp] = name
	}

	// errors.New / fmt.Errorf carry only their message, so the message decides.
	e1 := rec("api", fn, "reconcile failed", LogKeyExceptionType, "*errors.errorString", LogKeyExceptionMessage, "ledger drift")
	e2 := rec("api", fn, "reconcile failed", LogKeyExceptionType, "*errors.errorString", LogKeyExceptionMessage, "missing invoice")
	if ServerFingerprint(e1) == ServerFingerprint(e2) {
		t.Error("two errors.New messages at one site merged into one issue")
	}

	// error.group pins the issue whatever else changes, within a service.
	g1 := rec("api", fn, "a", LogKeyErrorGroup, "billing-drift")
	g2 := rec("api", fn+"X", "b", LogKeyErrorGroup, "billing-drift", LogKeyExceptionType, "*x.Y")
	if ServerFingerprint(g1) != ServerFingerprint(g2) {
		t.Error("error.group did not pin the issue")
	}
	if ServerFingerprint(g1) == ServerFingerprint(rec("worker", fn, "a", LogKeyErrorGroup, "billing-drift")) {
		t.Error("error.group merged two services")
	}

	// No source location: the instrumentation scope stands in for the function.
	n1 := ServerErrorRecord{ServiceName: "api", ScopeName: "otelcol", Body: "export failed"}
	n2 := ServerErrorRecord{ServiceName: "api", ScopeName: "temporal", Body: "export failed"}
	if ServerFingerprint(n1) == ServerFingerprint(n2) {
		t.Error("ScopeName is not the fallback for a record with no function")
	}
}

// The algorithm is frozen: stored issues are keyed by it. A change here is sfp2.
func TestServerFingerprintIsStable(t *testing.T) {
	r := rec("api", "github.com/acme/app/internal/handlers/jobs.(*Service).ListJobs", "list jobs failed",
		LogKeyExceptionType, "*pgconn.PgError", LogKeyProcedure, "/jobs.v1.JobService/ListJobs")
	const want = "46eff145a9e76695"
	if got := ServerFingerprint(r); got != want {
		t.Fatalf("sfp1 changed: %s, want %s. Bump to sfp2 (ServerFingerprintVersion) instead of editing sfp1.", got, want)
	}
	if len(want) != 16 {
		t.Fatal("sfp1 is 8 bytes of hex")
	}
}

func TestNormalizeMessage(t *testing.T) {
	for in, want := range map[string]string{
		"plain":                              "plain",
		"first\nsecond":                      "first",
		"GET https://a.example/x?y=1 failed": "GET <url> failed",
		"id 0b2f6f4e-5f6a-4c1b-9b1e-2a3b4c5d6e7f": "id <uuid>",
		`user 'bob' said "hi"`:                    "user <str> said <str>",
		"commit deadbeefcafe0123 and 42 rows":     "commit <hex> and <n> rows",
		"v2 retry3":                               "v2 retry3",
		strings.Repeat("x", 250):                  strings.Repeat("x", 200),
	} {
		if got := NormalizeMessage(in); got != want {
			t.Errorf("NormalizeMessage(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestServerTitleAndCulprit(t *testing.T) {
	r := rec("api", "pkg.F", "boom 12", LogKeyExceptionType, "*pgconn.PgError")
	if got := ServerTitle(r); got != "*pgconn.PgError: boom <n>" {
		t.Errorf("title %q", got)
	}
	if got := ServerCulprit(r); got != "internal/x/x.go in pkg.F" {
		t.Errorf("culprit %q", got)
	}
	bare := ServerErrorRecord{ScopeName: "scope", Body: "x"}
	if ServerTitle(bare) != "x" || ServerCulprit(bare) != "scope" {
		t.Errorf("bare record: %q / %q", ServerTitle(bare), ServerCulprit(bare))
	}
}

// The SQL never reads a line number, and evaluates both normalizations.
func TestServerFingerprintSQLShape(t *testing.T) {
	if strings.Contains(ServerFingerprintSQL, "line") {
		t.Error("sfp1 reads a line number")
	}
	if n := strings.Count(ServerFingerprintSQL, "replaceRegexpAll(splitByChar"); n != 2 {
		t.Errorf("expected norm(Body) and norm(exception.message), found %d normalizations", n)
	}
	if MessageNormSQL("Body") != sqlBodyNorm {
		t.Error("MessageNormSQL drifted from the expression sfp1 uses")
	}
}

// A server fault's issue is keyed by where it began, not by the function that
// logged it. Two internal errors with the same message, type and log site but
// different origins must be two issues; one origin is one issue however many
// requests hit it; and a client error is never given a stack.
//
//go:noinline
func faultAtListJobs() error { return svcerr.Internal("query failed") }

//go:noinline
func faultAtGetJob() error { return svcerr.Internal("query failed") }

// logAsInterceptor writes the record the way LoggingInterceptor does: every
// failure of every rpc is logged from this one function.
//
//go:noinline
func logAsInterceptor(l *slog.Logger, err error) { l.Error("rpc failed", "error", err) }

func sfp1Of(t *testing.T, err error) (string, map[string]string) {
	t.Helper()
	var buf bytes.Buffer
	logger := slog.New(observe.NewLogHandler(slog.NewJSONHandler(&buf, nil), observe.Config{}))
	logAsInterceptor(logger, err)
	var line map[string]any
	if jerr := json.Unmarshal(buf.Bytes(), &line); jerr != nil {
		t.Fatalf("log line: %v: %s", jerr, buf.String())
	}
	attrs := map[string]string{}
	for k, v := range line {
		if s, ok := v.(string); ok {
			attrs[k] = s
		}
		if f, ok := v.(float64); ok {
			attrs[k] = strconv.Itoa(int(f))
		}
	}
	return ServerFingerprint(ServerErrorRecord{ServiceName: "api", ScopeName: "github.com/acme/app", Body: "rpc failed", Attributes: attrs}), attrs
}

func TestServerFingerprintUsesTheTopApplicationFrameOfTheStack(t *testing.T) {
	fpList, listAttrs := sfp1Of(t, svcerr.Wrap(faultAtListJobs()))
	fpGet, getAttrs := sfp1Of(t, svcerr.Wrap(faultAtGetJob()))

	if fpList == fpGet {
		t.Fatalf("two internal errors from different sites share fingerprint %s", fpList)
	}
	if !strings.HasSuffix(listAttrs[LogKeyCodeFunction], ".faultAtListJobs") || !strings.HasSuffix(getAttrs[LogKeyCodeFunction], ".faultAtGetJob") {
		t.Errorf("code.function.name is not the stack's top application frame: %q, %q",
			listAttrs[LogKeyCodeFunction], getAttrs[LogKeyCodeFunction])
	}
	if !strings.Contains(listAttrs["exception.stacktrace"], "faultAtListJobs") {
		t.Errorf("record has no stack: %v", listAttrs)
	}

	// Same origin, a new request: still one issue.
	again, _ := sfp1Of(t, svcerr.Wrap(faultAtListJobs()))
	if again != fpList {
		t.Errorf("one origin split into two issues: %s vs %s", again, fpList)
	}

	// The control: with no stack on either record they share the log site and
	// collapse into one issue, which is the defect this fixes.
	for _, attrs := range []map[string]string{listAttrs, getAttrs} {
		delete(attrs, "exception.stacktrace")
		attrs[LogKeyCodeFunction] = "github.com/reliant-labs/forge/pkg/obsquery.logAsInterceptor"
	}
	a := ServerFingerprint(ServerErrorRecord{ServiceName: "api", Body: "rpc failed", Attributes: listAttrs})
	b := ServerFingerprint(ServerErrorRecord{ServiceName: "api", Body: "rpc failed", Attributes: getAttrs})
	if a != b {
		t.Errorf("control: with the log site as the frame these should merge, got %s and %s", a, b)
	}
}

func TestServerFingerprintOfAClientErrorHasNoStack(t *testing.T) {
	_, attrs := sfp1Of(t, svcerr.Wrap(svcerr.InvalidArgument("bad field")))
	if _, ok := attrs["exception.stacktrace"]; ok {
		t.Errorf("a client error carries a stack: %v", attrs)
	}
	if !strings.HasSuffix(attrs[LogKeyCodeFunction], ".logAsInterceptor") {
		t.Errorf("a client error's site is the log call site, got %q", attrs[LogKeyCodeFunction])
	}
}
