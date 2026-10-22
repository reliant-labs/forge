package secretaccess

import (
	"strings"
	"testing"
)

func writerRequest() WriterRequest {
	return WriterRequest{
		Scope:     Scope{Org: "acme", Env: "prod"},
		Cluster:   Cluster{ID: "hub", Audience: WriterAudience("hub")},
		Namespace: "openbao",
	}
}

// The writer can create and update under the env's directory and read its
// metadata. It can never read a value, and nothing outside the directory is
// reachable.
func TestDeriveWriter_WriteOnlyOnTheEnvDirectory(t *testing.T) {
	doc, err := DeriveWriter(writerRequest())
	if err != nil {
		t.Fatal(err)
	}
	if len(doc.Policies) != 1 || len(doc.Roles) != 1 || len(doc.Mounts) != 1 {
		t.Fatalf("document = %+v, want one mount, role and policy", doc)
	}
	got := map[string][]string{}
	for _, r := range doc.Policies[0].Rules {
		got[r.Path] = r.Capabilities
	}
	want := map[string][]string{
		"secret/data/tenants/acme/prod/*":     {"create", "update"},
		"secret/metadata/tenants/acme/prod/*": {"read", "list"},
	}
	if len(got) != len(want) {
		t.Fatalf("rules = %v, want exactly %v", got, want)
	}
	for path, caps := range want {
		if strings.Join(got[path], ",") != strings.Join(caps, ",") {
			t.Errorf("%s = %v, want %v", path, got[path], caps)
		}
	}
	for path, caps := range got {
		for _, c := range caps {
			if path == "secret/data/tenants/acme/prod/*" && c == "read" {
				t.Errorf("the writer can read values: %s %v", path, caps)
			}
			if c == "delete" || c == "sudo" || c == "patch" {
				t.Errorf("%s grants %q", path, c)
			}
		}
	}
}

func TestDeriveWriter_RoleBindsAccountAndAudience(t *testing.T) {
	doc, err := DeriveWriter(writerRequest())
	if err != nil {
		t.Fatal(err)
	}
	r := doc.Roles[0]
	if r.Mount != "jwt-hub" || r.BoundSubject != "system:serviceaccount:openbao:forge-secret-writer" {
		t.Errorf("role = %+v", r)
	}
	if len(r.BoundAudiences) != 1 || r.BoundAudiences[0] != "forge-secret-writer/hub" {
		t.Errorf("audiences = %v", r.BoundAudiences)
	}
	if r.Name != doc.Policies[0].Name || len(r.Policies) != 1 || r.Policies[0] != r.Name {
		t.Errorf("role %q carries policies %v, want exactly its own", r.Name, r.Policies)
	}
}

// A reader's token must not log in as the writer, nor the reverse: the audience
// and the name both differ.
func TestDeriveWriter_NeverCollidesWithAReader(t *testing.T) {
	scope := Scope{Org: "acme", Env: "prod"}
	readers, err := Derive(Request{
		Scope:    scope,
		Clusters: []Cluster{{ID: "hub", Audience: "https://secrets.example.com/c/hub"}},
		Bindings: []Binding{{Cluster: "hub", Namespace: "openbao", Names: []string{"A"}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	writer, err := DeriveWriter(writerRequest())
	if err != nil {
		t.Fatal(err)
	}
	if readers.Roles[0].Name == writer.Roles[0].Name {
		t.Errorf("reader and writer share the role name %q", writer.Roles[0].Name)
	}
	if !strings.Contains(writer.Roles[0].Name, "-w-") {
		t.Errorf("writer role %q lacks the writer marker", writer.Roles[0].Name)
	}
}

func TestDeriveWriter_NamesAreScopedPerEnv(t *testing.T) {
	a := writerRequest()
	b := writerRequest()
	b.Scope.Env = "staging"
	da, _ := DeriveWriter(a)
	db, _ := DeriveWriter(b)
	if da.Roles[0].Name == db.Roles[0].Name {
		t.Errorf("two envs share a writer role: %q", da.Roles[0].Name)
	}
}

func TestDeriveWriter_RefusesAnEscapingScope(t *testing.T) {
	for name, mutate := range map[string]func(*WriterRequest){
		"slash in the env":        func(w *WriterRequest) { w.Scope.Env = "prod/../other" },
		"glob in the org":         func(w *WriterRequest) { w.Scope.Org = "ac*" },
		"namespace not a label":   func(w *WriterRequest) { w.Namespace = "Open_Bao" },
		"account not a label":     func(w *WriterRequest) { w.ServiceAccount = "writer/x" },
		"cluster id with a slash": func(w *WriterRequest) { w.Cluster.ID = "hub/x" },
	} {
		t.Run(name, func(t *testing.T) {
			w := writerRequest()
			mutate(&w)
			if _, err := DeriveWriter(w); err == nil {
				t.Error("accepted a request whose policy could leave the env's directory")
			}
		})
	}
}

func TestWriterLogin_MatchesTheDerivedRole(t *testing.T) {
	w := writerRequest()
	login, err := w.Login()
	if err != nil {
		t.Fatal(err)
	}
	doc, _ := DeriveWriter(w)
	if login.Role != doc.Roles[0].Name || login.Audience != doc.Roles[0].BoundAudiences[0] {
		t.Errorf("login %+v disagrees with role %+v", login, doc.Roles[0])
	}
	if login.LoginPath != "auth/jwt-hub/login" || login.Namespace != "openbao" || login.ServiceAccount != "forge-secret-writer" {
		t.Errorf("login = %+v", login)
	}
}

func TestDeriveWriter_DefaultsTheAudiencePerCluster(t *testing.T) {
	w := writerRequest()
	w.Cluster.Audience = ""
	doc, err := DeriveWriter(w)
	if err != nil {
		t.Fatal(err)
	}
	if got := doc.Roles[0].BoundAudiences[0]; got != WriterAudience("hub") {
		t.Errorf("audience = %q, want %q", got, WriterAudience("hub"))
	}
}
