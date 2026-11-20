package ciguard

import (
	"os"
	"os/exec"
	"strings"
	"testing"
)

func TestCandidatePublicGitHubMetadataFailsClosed(t *testing.T) {
	if testing.Short() {
		t.Skip("runs the public API shell adapter with fixture curl; runs in task test")
	}
	requireCICommands(t, "bash", "jq")
	for _, tc := range []struct {
		name, mode, endpoint string
		args                 []string
		want                 string
		fail                 bool
	}{
		{"live PR fields", "", "pulls/7", nil, `"body":"proof"`, false},
		{"compat label query", "", "issues/7/labels", []string{"--jq", `[.[] | select(.name == "breaking:approved")] | length`}, "1", false},
		{"compat actor query yields unquoted login", "", "issues/7/events", []string{"--jq", `.[] | .actor.login`}, "\nlater\n", false},
		{"every event page", "", "issues/7/events", nil, `"login":"later"`, false},
		{"do not trust partial history", "rate-limit", "issues/7/events", nil, "rate limited", true},
		{"reject malformed labels response", "malformed", "issues/7/labels", nil, "", true},
		{"reject nonpublic endpoint", "", "actions/runs/7", nil, "", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			writeCIFixture(t, dir, "curl", []byte(fakeAnonymousCurl), 0o700)
			args := []string{"../../scripts/ci-public-gh.sh", "api"}
			if strings.HasSuffix(tc.endpoint, "/events") {
				args = append(args, "--paginate")
			}
			args = append(args, "repos/reliant-labs/forge/"+tc.endpoint)
			args = append(args, tc.args...)
			cmd := exec.Command("bash", args...)
			cmd.Env = append(os.Environ(), "PATH="+dir+string(os.PathListSeparator)+os.Getenv("PATH"), "MODE="+tc.mode, "GH_TOKEN=ignored-by-adapter")
			got, err := cmd.CombinedOutput()
			if (err != nil) != tc.fail || !strings.Contains(string(got), tc.want) {
				t.Fatalf("want failure=%v output=%q, got %v\n%s", tc.fail, tc.want, err, got)
			}
		})
	}
}

const fakeAnonymousCurl = `#!/usr/bin/env bash
set -euo pipefail
[[ $1 == -q && $* != *'Authorization:'* && $* != *'ignored-by-adapter'* ]]
case "${!#}" in
  https://api.github.com/repos/reliant-labs/forge/pulls/7\?*) echo '{"body":"proof","labels":[]}' ;;
  https://api.github.com/repos/reliant-labs/forge/issues/7/labels\?*)
    if [[ $MODE == malformed ]]; then echo '{"not":"labels"}'; else echo '[{"name":"breaking:approved"}]'; fi ;;
  https://api.github.com/repos/reliant-labs/forge/issues/7/events\?per_page=100\&page=1)
    jq -cn '[range(100) | {event:"labeled",actor:{login:"first"}}]' ;;
  https://api.github.com/repos/reliant-labs/forge/issues/7/events\?per_page=100\&page=2)
    if [[ $MODE == rate-limit ]]; then echo 'rate limited' >&2; exit 22; fi
    echo '[{"event":"labeled","actor":{"login":"later"}}]' ;;
  *) exit 12 ;;
esac
`
