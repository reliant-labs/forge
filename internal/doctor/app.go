package doctor

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"sort"
	"strings"
	"time"
)

// CheckAppHealth verifies the application's healthz and readyz endpoints.
//
// The address is the one the CALLER resolved (RuntimeTarget.HTTP) — this
// check never guesses a port. When nothing resolved one, the answer is
// UNDETERMINED, not a skip: "forge could not find the app" and "this project
// has no app" are different reports and must not render alike.
func CheckAppHealth(ctx context.Context, env *Environment) CheckResult {
	addr, ok := env.GetPort("app", 8080)
	if !ok {
		return CheckResult{
			Status: StatusUnknown,
			Message: "could not resolve an HTTP address for any host service" +
				" — is the stack up? (`forge env up <env>`)",
		}
	}

	client := &http.Client{Timeout: 3 * time.Second}
	// Name the service that answered. A project runs several host services;
	// "healthz=ok" without a subject says nothing about which one.
	subject := addr
	if env.Target.Service != "" {
		subject = env.Target.Service + " " + addr
	}

	// Check /healthz
	healthzBody, err := httpGetBody(ctx, client, fmt.Sprintf("http://%s/healthz", addr))
	if err != nil {
		return CheckResult{Status: StatusFail, Message: fmt.Sprintf("%s: healthz failed: %v", subject, err)}
	}
	healthzStr := strings.TrimSpace(healthzBody)
	if healthzStr != "ok" {
		return CheckResult{
			Status:   StatusFail,
			Message:  fmt.Sprintf("%s: healthz returned %q, expected \"ok\"", subject, healthzStr),
			Evidence: healthzBody,
		}
	}

	// Check /readyz
	_, err = httpGetBody(ctx, client, fmt.Sprintf("http://%s/readyz", addr))
	if err != nil {
		return CheckResult{Status: StatusFail, Message: fmt.Sprintf("%s: readyz failed: %v", subject, err)}
	}

	return CheckResult{Status: StatusPass, Message: subject + ": healthz=ok readyz=ok"}
}

// CheckPprof verifies the pprof debug endpoint is reachable and reports
// the available profile types.
func CheckPprof(ctx context.Context, env *Environment) CheckResult {
	addr, ok := env.GetPort("app", 6060)
	if !ok {
		return CheckResult{
			Status: StatusUnknown,
			Message: "could not resolve a pprof address" +
				" — declare PPROF_ADDR on the host service (serverkit binds pprof on its own listener)",
		}
	}

	client := &http.Client{Timeout: 3 * time.Second}

	body, err := httpGetBody(ctx, client, fmt.Sprintf("http://%s/debug/pprof/", addr))
	if err != nil {
		return CheckResult{Status: StatusFail, Message: fmt.Sprintf("pprof failed: %v", err)}
	}

	// Parse rows like: <tr><td>91</td><td><a href='allocs?debug=1'>allocs</a></td></tr>
	re := regexp.MustCompile(`<td>(\d+)</td><td><a href='[^']*'>([^<]+)</a></td>`)
	matches := re.FindAllStringSubmatch(body, -1)

	if len(matches) == 0 {
		return CheckResult{
			Status:   StatusPass,
			Message:  "pprof reachable (no profile rows parsed)",
			Evidence: body,
		}
	}

	var parts []string
	for _, m := range matches {
		parts = append(parts, fmt.Sprintf("%s=%s", m[2], m[1]))
	}
	sort.Strings(parts)

	return CheckResult{
		Status:  StatusPass,
		Message: strings.Join(parts, " "),
	}
}

// httpGetBody performs a GET request and returns the response body as a string.
// It returns an error if the status code is not 200.
func httpGetBody(ctx context.Context, client *http.Client, url string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, http.NoBody)
	if err != nil {
		return "", err
	}

	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer func() { _ = resp.Body.Close() }()

	b, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}

	if resp.StatusCode != http.StatusOK {
		return string(b), fmt.Errorf("status %d", resp.StatusCode)
	}

	return string(b), nil
}
