package cli

import (
	"regexp"
	"strings"
	"testing"
)

// expoCheckVerdict is what an Expo project check's output and exit code say.
type expoCheckVerdict int

const (
	// expoCheckPassed: the command exited 0.
	expoCheckPassed expoCheckVerdict = iota
	// expoCheckFailed: the command found a real problem in the project (or
	// failed in a way this classifier cannot attribute to the network).
	expoCheckFailed
	// expoCheckUnreachable: the only thing wrong is that Expo's service could
	// not be reached, which is the host's fault, not the project's.
	expoCheckUnreachable
)

func (v expoCheckVerdict) String() string {
	return [...]string{"passed", "failed", "unreachable"}[v]
}

// expoNetworkError matches a connection failure to Expo's API as Node and
// expo-doctor print it. Each alternative was captured from a real run with
// outbound network denied (macOS sandbox-exec) or refused (dead proxy).
var expoNetworkError = regexp.MustCompile(
	`TypeError: fetch failed|connect (ECONNREFUSED|EPERM|ENETUNREACH|EHOSTUNREACH|ETIMEDOUT)|` +
		`getaddrinfo (ENOTFOUND|EAI_AGAIN)|ECONNRESET|AggregateError \[(EPERM|ECONNREFUSED|ETIMEDOUT)\]|` +
		`requires a connection to the Expo API`)

// expoDoctorDirectoryCheck is the one Doctor check that reports a failed
// request to Expo's React Native Directory without an "Unexpected error" block.
const (
	expoDoctorDirectoryCheck  = "Validate packages against React Native Directory package metadata"
	expoDoctorDirectoryOutage = "Directory check failed with unexpected server response"
)

var expoDoctorFailedCheck = regexp.MustCompile(`(?m)^✖ (.+?)\s*$`)

// classifyExpoDoctor classifies `expo-doctor`. A failed run is unreachable only
// when EVERY failed check is explained by a failed request to Expo; one real
// failed check among network ones is still a failure.
func classifyExpoDoctor(output string, exitCode int) expoCheckVerdict {
	if exitCode == 0 {
		return expoCheckPassed
	}
	failed := expoDoctorFailedCheck.FindAllStringSubmatch(output, -1)
	if len(failed) == 0 {
		return expoCheckFailed
	}
	for _, m := range failed {
		if !expoDoctorCheckNetworkCaused(output, m[1]) {
			return expoCheckFailed
		}
	}
	return expoCheckUnreachable
}

func expoDoctorCheckNetworkCaused(output, check string) bool {
	if check == expoDoctorDirectoryCheck && strings.Contains(output, expoDoctorDirectoryOutage) {
		return true
	}
	header := "Unexpected error while running '" + check + "' check:"
	_, after, found := strings.Cut(output, header)
	if !found {
		return false
	}
	// The error text runs to the next blank-or-summary line; the check's own
	// block is short, so bound the search instead of scanning the whole output.
	if len(after) > 600 {
		after = after[:600]
	}
	return expoNetworkError.MatchString(after)
}

// classifyExpoInstallCheck classifies `expo install --check`. Its real-failure
// verdict ("Found outdated dependencies") always wins over a network pattern.
func classifyExpoInstallCheck(output string, exitCode int) expoCheckVerdict {
	if exitCode == 0 {
		return expoCheckPassed
	}
	if strings.Contains(output, "Found outdated dependencies") || strings.Contains(output, "should be updated for best compatibility") {
		return expoCheckFailed
	}
	if expoNetworkError.MatchString(output) {
		return expoCheckUnreachable
	}
	return expoCheckFailed
}

// Samples are real output, captured against expo-doctor 1.21.6 and expo 58.0.23
// on a scaffolded mobile app (ANSI stripped). "unreachable" ones ran with
// outbound network denied by macOS sandbox-exec, or through a refusing proxy;
// "failed" ones ran with react-native-screens ~4.10.0 and expo-constants ~17.0.0
// installed into an SDK 58 app.
const (
	sampleDoctorPassed = "Running 20 checks on your project...\n20/20 checks passed. No issues detected!\n"

	sampleDoctorUnreachable = `Running 20 checks on your project...
Unexpected error while running 'Check Expo config (app.json/ app.config.js) schema' check:
TypeError: fetch failed
Error: connect EPERM 34.110.201.56:443 - Local (0.0.0.0:0)
This check requires a connection to the Expo API. Ensure your network connection is stable.
Warning: Could not fetch bundled native modules
18/20 checks passed. 2 checks failed. Possible issues detected:
Use the --verbose flag to see more details about passed checks.

✖ Check Expo config (app.json/ app.config.js) schema
✖ Validate packages against React Native Directory package metadata
Directory check failed with unexpected server response

2 checks failed, indicating possible issues with the project.
`

	sampleDoctorFailed = `Running 20 checks on your project...
18/20 checks passed. 2 checks failed. Possible issues detected:
Use the --verbose flag to see more details about passed checks.

✖ Check that no duplicate dependencies are installed
Your project contains duplicate native module dependencies, which should be de-duplicated.
Found duplicates for expo-constants:
  ├─ expo-constants@17.0.8 (at: node_modules/expo-constants)
  └─ expo-constants@58.0.15 (at: node_modules/.pnpm/expo-constants@58.0.15/node_modules/expo-constants)

✖ Check that packages match versions required by installed Expo SDK

❗ Major version mismatches
package               expected  found
expo-constants        ~58.0.15  17.0.8

2 packages out of date.

2 checks failed, indicating possible issues with the project.
`

	// One network-caused failure alongside one real one: the real one decides.
	sampleDoctorMixed = `Running 20 checks on your project...
Unexpected error while running 'Check Expo config (app.json/ app.config.js) schema' check:
TypeError: fetch failed
Error: connect EPERM 34.110.201.56:443 - Local (0.0.0.0:0)
This check requires a connection to the Expo API. Ensure your network connection is stable.
17/20 checks passed. 2 checks failed. Possible issues detected:

✖ Check Expo config (app.json/ app.config.js) schema
✖ Check that no duplicate dependencies are installed
Found duplicates for expo-constants:

2 checks failed, indicating possible issues with the project.
`

	sampleInstallPassed = "Dependencies are up to date\n"

	sampleInstallSandbox = `AggregateError [EPERM]:
    at internalConnectMultiple (node:net:1430:18)
    at internalConnectMultiple (node:net:1506:5)
    at GetAddrInfoReqWrap.onlookupall [as oncomplete] (node:dns:134:8)
`

	sampleInstallRefused = `Error: connect ECONNREFUSED 127.0.0.1:9
Error: connect ECONNREFUSED 127.0.0.1:9
    at TCPConnectWrap.afterConnect [as oncomplete] (node:net:2021:16)
`

	sampleInstallOutdated = `The following packages should be updated for best compatibility with the installed expo version:
  expo-constants@17.0.8 - expected version: ~58.0.15
  react-native-screens@4.10.0 - expected version: ~4.30.1
Your project may not work correctly until you install the expected versions of the packages.
Found outdated dependencies
`
)

func TestClassifyExpoChecks(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		classify func(string, int) expoCheckVerdict
		output   string
		exit     int
		want     expoCheckVerdict
	}{
		{"doctor passed", classifyExpoDoctor, sampleDoctorPassed, 0, expoCheckPassed},
		{"doctor offline sandbox", classifyExpoDoctor, sampleDoctorUnreachable, 1, expoCheckUnreachable},
		{"doctor real failures", classifyExpoDoctor, sampleDoctorFailed, 1, expoCheckFailed},
		{"doctor real failure beside a network one", classifyExpoDoctor, sampleDoctorMixed, 1, expoCheckFailed},
		{"doctor crash with no check summary", classifyExpoDoctor, "TypeError: Cannot read properties of undefined\n", 1, expoCheckFailed},
		{"doctor nonzero exit with no output", classifyExpoDoctor, "", 1, expoCheckFailed},
		{"install check passed", classifyExpoInstallCheck, sampleInstallPassed, 0, expoCheckPassed},
		{"install check offline sandbox", classifyExpoInstallCheck, sampleInstallSandbox, 1, expoCheckUnreachable},
		{"install check refused connection", classifyExpoInstallCheck, sampleInstallRefused, 1, expoCheckUnreachable},
		{"install check outdated", classifyExpoInstallCheck, sampleInstallOutdated, 1, expoCheckFailed},
		{"install check outdated beats network text", classifyExpoInstallCheck, sampleInstallRefused + sampleInstallOutdated, 1, expoCheckFailed},
		{"install check unrecognized failure", classifyExpoInstallCheck, "Error: ERR_PNPM_OUTDATED_LOCKFILE\n", 1, expoCheckFailed},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := tt.classify(tt.output, tt.exit); got != tt.want {
				t.Errorf("verdict = %v, want %v\n%s", got, tt.want, tt.output)
			}
		})
	}
}
