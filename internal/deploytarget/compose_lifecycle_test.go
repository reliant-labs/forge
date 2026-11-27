package deploytarget

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/reliant-labs/forge/internal/statefile"
)

func TestComposeDownExactCheckoutEnvAndRecordedContainers(t *testing.T) {
	dir := t.TempDir()
	record := composeOwned{Directory: dir, Checkout: dir, Env: "dev", Project: "project-dev-feature", Service: "clickstack", IDs: []string{"ours", "reused"}}
	for _, env := range []string{"dev", "other"} {
		if err := statefile.Write(composeOwnedPath(dir, env, "clickstack"), "compose ownership", record); err != nil {
			t.Fatal(err)
		}
	}
	label := func(directory, project string) string {
		b, _ := json.Marshal([]map[string]any{{"Config": map[string]any{"Labels": map[string]string{"com.docker.compose.project": project, "com.docker.compose.project.working_dir": directory, "com.docker.compose.service": "clickstack"}}}})
		return string(b)
	}
	r := &fakeRunner{outputs: map[string]string{"docker inspect ours": label(dir, record.Project), "docker inspect reused": label(filepath.Join(dir, "another"), record.Project)}}
	p := ComposeProvider{ProjectDir: dir, Runner: r}
	if err := p.StopOwned(context.Background(), "dev"); err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(r.calls, "\n")
	if !strings.Contains(joined, "docker rm -f ours") {
		t.Fatalf("owned container forgotten: %s", joined)
	}
	if strings.Contains(joined, "rm -f reused") || strings.Contains(joined, "-v") || strings.Contains(joined, "down") {
		t.Fatalf("overbroad teardown: %s", joined)
	}
	other, err := statefile.Read[composeOwned](composeOwnedPath(dir, "other", "clickstack"), "compose ownership")
	if err != nil || other == nil {
		t.Fatalf("other env record lost: %v", err)
	}
}

// TestComposeDownSurvivesAGoneOrUnverifiableContainer pins the env-down
// regression from a ClickStack + otel-profiles stack: compose had replaced a
// recorded container, Docker 29 reported it as "error: no such object" (lower
// case), the check treated that as a failure, and StopOwned returned before
// reaching otel-profiles and profiles-schema, which kept running.
func TestComposeDownSurvivesAGoneOrUnverifiableContainer(t *testing.T) {
	dir := t.TempDir()
	const project = "forge-abc-dev"
	write := func(service string, ids ...string) {
		t.Helper()
		services := map[string]string{}
		for _, id := range ids {
			services[id] = service
		}
		rec := composeOwned{Directory: dir, Checkout: dir, Env: "dev", Project: project, Service: service, IDs: ids, Services: services}
		if err := statefile.Write(composeOwnedPath(dir, "dev", service), "compose ownership", rec); err != nil {
			t.Fatal(err)
		}
	}
	// Records glob in name order, so the failures sit in the FIRST record:
	// stopping there is exactly the leak this test exists for.
	write("clickstack", "a-replaced", "b-docker28-gone", "c-clickstack", "d-unverifiable")
	write("otel-profiles", "e-otel", "f-foreign")
	write("profiles-schema", "g-exited-schema")

	label := func(directory, project, service string) string {
		b, _ := json.Marshal([]map[string]any{{"Config": map[string]any{"Labels": map[string]string{"com.docker.compose.project": project, "com.docker.compose.project.working_dir": directory, "com.docker.compose.service": service}}}})
		return string(b)
	}
	r := &fakeRunner{
		outputs: map[string]string{
			"docker inspect a-replaced":      "[]\nerror: no such object: a-replaced\n",
			"docker inspect b-docker28-gone": "[]\nError: No such object: b-docker28-gone\n",
			"docker inspect c-clickstack":    label(dir, project, "clickstack"),
			"docker inspect d-unverifiable":  "permission denied while trying to connect to the Docker daemon socket",
			"docker inspect e-otel":          label(dir, project, "otel-profiles"),
			"docker inspect f-foreign":       label(dir, "someone-else", "otel-profiles"),
			"docker inspect g-exited-schema": label(dir, project, "profiles-schema"),
		},
		runErrs: map[string]error{
			"docker inspect a-replaced":      errors.New("exit status 1"),
			"docker inspect b-docker28-gone": errors.New("exit status 1"),
			"docker inspect d-unverifiable":  errors.New("exit status 1"),
		},
	}
	p := ComposeProvider{ProjectDir: dir, Runner: r}
	err := p.StopOwned(context.Background(), "dev")
	if err == nil || !strings.Contains(err.Error(), "d-unverifiable") {
		t.Fatalf("an unverifiable container must be reported, got %v", err)
	}
	if strings.Contains(err.Error(), "a-replaced") || strings.Contains(err.Error(), "b-docker28-gone") {
		t.Fatalf("a container that no longer exists is not a failure: %v", err)
	}

	removed := map[string]bool{}
	for _, call := range r.calls {
		if id, ok := strings.CutPrefix(call, "docker rm -f "); ok {
			removed[id] = true
		}
	}
	for _, id := range []string{"c-clickstack", "e-otel", "g-exited-schema"} {
		if !removed[id] {
			t.Errorf("owned container %s left running; calls:\n%s", id, strings.Join(r.calls, "\n"))
		}
	}
	for _, id := range []string{"a-replaced", "b-docker28-gone", "d-unverifiable", "f-foreign"} {
		if removed[id] {
			t.Errorf("removed %s, whose ownership was not proven", id)
		}
	}

	// The unverified ID stays recorded so the next down retries it; settled
	// records are gone.
	rec, err := statefile.Read[composeOwned](composeOwnedPath(dir, "dev", "clickstack"), "compose ownership")
	if err != nil || rec == nil || !slices.Equal(rec.IDs, []string{"d-unverifiable"}) || len(rec.Services) != 1 {
		t.Fatalf("clickstack record should keep only the unverified ID: %+v %v", rec, err)
	}
	for _, service := range []string{"otel-profiles", "profiles-schema"} {
		if rec, err := statefile.Read[composeOwned](composeOwnedPath(dir, "dev", service), "compose ownership"); err != nil || rec != nil {
			t.Errorf("%s record should be deleted once settled: %+v %v", service, rec, err)
		}
	}
}

// composeNetworkFixture is one owned clickstack container of project, recorded
// for env dev, and the canned docker answers env down reads for it. Tests add
// the network answers they exercise.
func composeNetworkFixture(t *testing.T, project string) (string, *fakeRunner) {
	t.Helper()
	dir := t.TempDir()
	rec := composeOwned{Directory: dir, Checkout: dir, Env: "dev", Project: project, Service: "clickstack", IDs: []string{"c-owned"}, Networks: []string{"net-empty", "net-running", "net-stopped", "net-foreign", "net-external", "net-gone", "net-a", "net-b"}, Services: map[string]string{"c-owned": "clickstack"}}
	if err := statefile.Write(composeOwnedPath(dir, "dev", "clickstack"), "compose ownership", rec); err != nil {
		t.Fatal(err)
	}
	labels, _ := json.Marshal([]map[string]any{{"Config": map[string]any{"Labels": map[string]string{"com.docker.compose.project": project, "com.docker.compose.project.working_dir": dir, "com.docker.compose.service": "clickstack"}}}})
	return dir, &fakeRunner{outputs: map[string]string{"docker inspect c-owned": string(labels)}, runErrs: map[string]error{}}
}

// networkJSON is `docker network inspect` for one network.
func networkJSON(project, network string, attached ...string) string {
	containers := map[string]any{}
	for _, id := range attached {
		containers[id] = map[string]string{"Name": id}
	}
	labels := map[string]string{"com.docker.compose.project": project, "com.docker.compose.version": "5.8.0"}
	if network != "" {
		labels["com.docker.compose.network"] = network
	}
	b, _ := json.Marshal([]map[string]any{{"Name": project + "_" + network, "Labels": labels, "Containers": containers}})
	return string(b)
}

func networksRemoved(r *fakeRunner) map[string]bool {
	removed := map[string]bool{}
	for _, call := range r.calls {
		if id, ok := strings.CutPrefix(call, "docker network rm "); ok {
			removed[id] = true
		}
	}
	return removed
}

// Even an empty network with matching Compose labels and a recorded ID
// lacks proof that it is exclusive to this stack.
func TestComposeDownKeepsNetworksWithoutExclusiveOwnership(t *testing.T) {
	const project = "forge-abc-dev"
	dir, r := composeNetworkFixture(t, project)
	r.outputs["docker network ls --quiet --filter label=com.docker.compose.project="+project] = "net-empty\nnet-running\nnet-stopped\nnet-foreign\nnet-external\nnet-gone\n"
	r.outputs["docker network inspect net-empty"] = networkJSON(project, "shop-net")
	r.outputs["docker network inspect net-running"] = networkJSON(project, "db-net", "other-stack-container")
	r.outputs["docker network inspect net-stopped"] = networkJSON(project, "cache-net")
	r.outputs["docker container ls --all --quiet --filter network=net-stopped"] = "e7aa82ad0485\n"
	r.outputs["docker network inspect net-foreign"] = networkJSON("forge-xyz-dev", "shop-net") // the filter is checked, not trusted
	r.outputs["docker network inspect net-external"] = networkJSON(project, "")
	r.outputs["docker network inspect net-gone"] = "[]\nError response from daemon: network net-gone not found\n"
	r.runErrs["docker network inspect net-gone"] = errors.New("exit status 1")

	p := ComposeProvider{ProjectDir: dir, Runner: r}
	if err := p.StopOwned(context.Background(), "dev"); err != nil {
		t.Fatal(err)
	}
	removed := networksRemoved(r)
	for _, id := range []string{"net-empty", "net-running", "net-stopped", "net-foreign", "net-external", "net-gone"} {
		if removed[id] {
			t.Errorf("removed network %s, which is in use or not provably the project's", id)
		}
	}
	for _, call := range r.calls {
		if strings.Contains(call, "prune") || (strings.HasPrefix(call, "docker network ls") && !strings.HasSuffix(call, "label=com.docker.compose.project="+project)) {
			t.Errorf("overbroad network call: %s", call)
		}
	}
	if !slices.Contains(r.calls, "docker rm -f c-owned") {
		t.Errorf("owned container not removed; calls:\n%s", strings.Join(r.calls, "\n"))
	}
}

// TestComposeDownKeepsNetworksWhileTheProjectIsNotSettled: a container of the
// project left (unremovable, unverifiable, or not ours to remove) still
// references the networks, so none of them is even listed.
func TestComposeDownKeepsNetworksWhileTheProjectIsNotSettled(t *testing.T) {
	const project = "forge-abc-dev"
	for name, arrange := range map[string]func(r *fakeRunner){
		"container rm fails": func(r *fakeRunner) { r.runErrs["docker rm -f c-owned"] = errors.New("exit status 1") },
		"project container left": func(r *fakeRunner) {
			r.outputs["docker container ls --all --quiet --filter label=com.docker.compose.project="+project] = "f00dcafe\n"
		},
	} {
		t.Run(name, func(t *testing.T) {
			dir, r := composeNetworkFixture(t, project)
			r.outputs["docker network ls --quiet --filter label=com.docker.compose.project="+project] = "net-empty\n"
			r.outputs["docker network inspect net-empty"] = networkJSON(project, "shop-net")
			arrange(r)
			_ = (ComposeProvider{ProjectDir: dir, Runner: r}).StopOwned(context.Background(), "dev")
			for _, call := range r.calls {
				if strings.HasPrefix(call, "docker network ") {
					t.Errorf("touched networks of an unsettled project: %s", call)
				}
			}
		})
	}
}

// TestComposeDownNetworkFailuresDoNotFailEnvDown: the containers are gone,
// which is what env down promises; a network docker will not remove, or a
// daemon that stopped answering, is left for the next env down.
func TestComposeDownNetworkFailuresDoNotFailEnvDown(t *testing.T) {
	const project = "forge-abc-dev"
	inspect := "docker network inspect net-a"
	for name, arrange := range map[string]func(r *fakeRunner){
		"rm refused": func(r *fakeRunner) {
			r.outputs["docker network inspect net-a"] = networkJSON(project, "a-net")
			r.outputs["docker network inspect net-b"] = networkJSON(project, "b-net")
			r.outputs["docker network rm net-a"] = "Error response from daemon: error while removing network: network net-a has active endpoints\n"
			r.runErrs["docker network rm net-a"] = errors.New("exit status 1")
		},
		"daemon down": func(r *fakeRunner) {
			r.outputs[inspect] = "Cannot connect to the Docker daemon at unix:///var/run/docker.sock\n"
			r.runErrs[inspect] = errors.New("exit status 1")
		},
	} {
		t.Run(name, func(t *testing.T) {
			dir, r := composeNetworkFixture(t, project)
			arrange(r)
			if err := (ComposeProvider{ProjectDir: dir, Runner: r}).StopOwned(context.Background(), "dev"); err != nil {
				t.Fatalf("a network failure failed env down: %v", err)
			}
			if len(networksRemoved(r)) != 0 {
				t.Errorf("removed a network without exclusive ownership; calls:\n%s", strings.Join(r.calls, "\n"))
			}
		})
	}
}

func TestComposeDownKeepsUnrecordedNetworksWithTheSameProjectLabel(t *testing.T) {
	const project = "shared-name"
	dir, r := composeNetworkFixture(t, project)
	r.outputs["docker network ls --quiet --filter label=com.docker.compose.project="+project] = "net-empty\nother-checkout-net\n"
	r.outputs["docker network inspect net-empty"] = networkJSON(project, "default")
	r.outputs["docker network inspect other-checkout-net"] = networkJSON(project, "default")
	if err := (ComposeProvider{ProjectDir: dir, Runner: r}).StopOwned(context.Background(), "dev"); err != nil {
		t.Fatal(err)
	}
	removed := networksRemoved(r)
	if len(removed) != 0 {
		t.Fatalf("neither recorded nor unrecorded identity proves exclusivity: %v", removed)
	}
}

func TestComposeDownPreservesNetworkReusedByTwoInvocations(t *testing.T) {
	const project = "shared-name"
	const network = "concrete-network-id"
	r := &fakeRunner{outputs: map[string]string{
		"docker network inspect " + network: networkJSON(project, "default"),
	}}
	var providers []ComposeProvider
	for _, service := range []string{"app", "worker"} {
		dir := t.TempDir()
		spec := &ComposeSpec{ProjectDirectory: dir}
		id := service + "-container"
		inspect, err := json.Marshal([]map[string]any{{
			"Config": map[string]any{"Labels": map[string]string{
				"com.docker.compose.project":             project,
				"com.docker.compose.project.working_dir": dir,
				"com.docker.compose.service":             service,
			}},
			"NetworkSettings": map[string]any{"Networks": map[string]any{
				"shared-name_default": map[string]string{"NetworkID": network},
			}},
		}})
		if err != nil {
			t.Fatal(err)
		}
		r.outputs["docker inspect "+id] = string(inspect)
		ps := "docker " + strings.Join(append(composeArgs(spec), "ps", "--all", "--quiet"), " ")
		r.outputs[ps] = id + "\n"
		p := ComposeProvider{ProjectDir: dir, Runner: r}
		if err := p.recordOwned(context.Background(), r, nil, spec, "dev", service, nil); err != nil {
			t.Fatal(err)
		}
		rec, err := statefile.Read[composeOwned](composeOwnedPath(dir, "dev", service), "compose ownership")
		if err != nil || rec == nil || !slices.Equal(rec.Networks, []string{network}) {
			t.Fatalf("both invocations must record the same network: %+v, %v", rec, err)
		}
		providers = append(providers, p)
	}
	// Both invocations stop; the now-empty network must survive for either
	// invocation to start again. Matching labels and IDs are insufficient.
	for _, p := range providers {
		if err := p.StopOwned(context.Background(), "dev"); err != nil {
			t.Fatal(err)
		}
	}
	for _, id := range []string{"app-container", "worker-container"} {
		if !slices.Contains(r.calls, "docker rm -f "+id) {
			t.Errorf("owned container %s was not removed", id)
		}
	}
	if len(networksRemoved(r)) != 0 {
		t.Fatalf("deleted a network reused by another invocation: %v", r.calls)
	}
}

func TestComposeDownLegacyRecordDoesNotAdoptNetworks(t *testing.T) {
	const project = "shared-name"
	dir, r := composeNetworkFixture(t, project)
	path := composeOwnedPath(dir, "dev", "clickstack")
	rec, err := statefile.Read[composeOwned](path, "compose ownership")
	if err != nil || rec == nil {
		t.Fatalf("read fixture: %v", err)
	}
	rec.Networks = nil
	if err := statefile.Write(path, "compose ownership", rec); err != nil {
		t.Fatal(err)
	}
	r.outputs["docker network ls --quiet --filter label=com.docker.compose.project="+project] = "net-empty\n"
	r.outputs["docker network inspect net-empty"] = networkJSON(project, "default")
	if err := (ComposeProvider{ProjectDir: dir, Runner: r}).StopOwned(context.Background(), "dev"); err != nil {
		t.Fatal(err)
	}
	if len(networksRemoved(r)) != 0 {
		t.Fatal("a legacy record must not adopt networks by project label")
	}
}

func TestComposeRecordsNetworkIdentitiesFromVerifiedContainers(t *testing.T) {
	dir := t.TempDir()
	spec := &ComposeSpec{ProjectDirectory: dir}
	inspect, err := json.Marshal([]map[string]any{{
		"Config": map[string]any{"Labels": map[string]string{
			"com.docker.compose.project":             "shared-name",
			"com.docker.compose.project.working_dir": dir,
			"com.docker.compose.service":             "app",
		}},
		"NetworkSettings": map[string]any{"Networks": map[string]any{
			"shared-name_default": map[string]string{"NetworkID": "concrete-network-id"},
		}},
	}})
	if err != nil {
		t.Fatal(err)
	}
	ps := "docker " + strings.Join(append(composeArgs(spec), "ps", "--all", "--quiet"), " ")
	r := &fakeRunner{outputs: map[string]string{ps: "owned-container\n", "docker inspect owned-container": string(inspect)}}
	p := ComposeProvider{ProjectDir: dir, Runner: r}
	if err := p.recordOwned(context.Background(), r, nil, spec, "dev", "app", nil); err != nil {
		t.Fatal(err)
	}
	rec, err := statefile.Read[composeOwned](composeOwnedPath(dir, "dev", "app"), "compose ownership")
	if err != nil || rec == nil || !slices.Equal(rec.Networks, []string{"concrete-network-id"}) {
		t.Fatalf("network identities were not persisted: %+v, %v", rec, err)
	}
}

func TestContainerAbsentMatchesEveryDockerSpelling(t *testing.T) {
	for out, want := range map[string]bool{
		"error: no such object: 3f2a":                      true, // Docker 29
		"Error: No such object: 3f2a":                      true, // Docker ≤ 28
		"Error response from daemon: No such container: x": true,
		"permission denied while trying to connect":        false,
		"Cannot connect to the Docker daemon":              false,
	} {
		if got := containerAbsent([]byte(out)); got != want {
			t.Errorf("containerAbsent(%q) = %v, want %v", out, got, want)
		}
	}
}
