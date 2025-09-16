package cli

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/spf13/cobra"
)

// testFlags holds the flag values for the test command.
type testFlags struct {
	race     bool
	coverage bool
	parallel bool
	verbose  bool
	service  string
}

func newTestCmd() *cobra.Command {
	var flags testFlags

	testCmd := &cobra.Command{
		Use:   "test",
		Short: "Run all tests (unit + integration)",
		Long: `Run all tests across Go services and frontends.

When no subcommand is given, runs unit and integration tests.

Examples:
  forge test                        # Run unit + integration tests
  forge test --coverage             # Enable coverage reporting
  forge test --service api-gateway  # Test specific service only
  forge test -V                     # Verbose test output`,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runTestAll(flags)
		},
	}

	testUnitCmd := &cobra.Command{
		Use:   "unit",
		Short: "Run unit tests only",
		Long: `Run unit tests for Go services.

Runs: go test -race -count=1 ./handlers/...

Examples:
  forge test unit
  forge test unit -V
  forge test unit --service api-gateway`,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runTestUnit(flags)
		},
	}

	testIntegrationCmd := &cobra.Command{
		Use:   "integration",
		Short: "Run integration tests only",
		Long: `Run integration tests with the integration build tag.

Runs: go test -race -tags=integration ./handlers/...

Examples:
  forge test integration
  forge test integration --service api-gateway`,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runTestIntegration(flags)
		},
	}

	testE2ECmd := &cobra.Command{
		Use:   "e2e",
		Short: "Run end-to-end tests",
		Long: `Run end-to-end tests from the e2e/ directory.

Runs: go test -v ./e2e/...

These tests typically require running infrastructure (database, services, etc).

Examples:
  forge test e2e
  forge test e2e -V`,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runTestE2E(flags)
		},
	}

	testCmd.AddCommand(testUnitCmd)
	testCmd.AddCommand(testIntegrationCmd)
	testCmd.AddCommand(testE2ECmd)

	// Flags for the parent test command (inherited by subcommands)
	testCmd.PersistentFlags().BoolVar(&flags.race, "race", true, "Enable race detector")
	testCmd.PersistentFlags().BoolVar(&flags.coverage, "coverage", false, "Enable coverage reporting")
	testCmd.PersistentFlags().BoolVar(&flags.parallel, "parallel", true, "Run test suites in parallel")
	testCmd.PersistentFlags().BoolVarP(&flags.verbose, "test-verbose", "V", false, "Verbose test output")
	testCmd.PersistentFlags().StringVar(&flags.service, "service", "", "Test a specific service or frontend by name")

	return testCmd
}

// testResult holds the outcome of a single test run.
type testResult struct {
	Name     string
	Passed   bool
	Duration time.Duration
	Output   string
}

func runTestAll(flags testFlags) error {
	fmt.Println("[test] Running all tests (unit + integration)...")
	fmt.Println()

	var (
		results      []testResult
		discoveryErrs []error
	)

	// Run Go unit tests
	unitResults, err := runGoTests("./...", []string{"-count=1"}, flags)
	if err != nil {
		fmt.Fprintf(os.Stderr, "[test] Go unit test discovery failed: %v\n", err)
		discoveryErrs = append(discoveryErrs, fmt.Errorf("unit test discovery: %w", err))
	}
	results = append(results, unitResults...)

	// Run Go integration tests
	integrationResults, err := runGoTests("./...", []string{"-tags", "integration"}, flags)
	if err != nil {
		fmt.Fprintf(os.Stderr, "[test] Go integration test discovery failed: %v\n", err)
		discoveryErrs = append(discoveryErrs, fmt.Errorf("integration test discovery: %w", err))
	}
	results = append(results, integrationResults...)

	// Run frontend tests if frontends/ exists
	frontendResults := runFrontendTests(flags)
	results = append(results, frontendResults...)

	return printTestSummary(results, discoveryErrs)
}

func runTestUnit(flags testFlags) error {
	fmt.Println("[test] Running unit tests...")
	fmt.Println()

	var discoveryErrs []error
	results, err := runGoTests("./...", []string{"-count=1"}, flags)
	if err != nil {
		fmt.Fprintf(os.Stderr, "[test] Go unit test discovery failed: %v\n", err)
		discoveryErrs = append(discoveryErrs, fmt.Errorf("unit test discovery: %w", err))
	}

	// Also run frontend tests
	frontendResults := runFrontendTests(flags)
	results = append(results, frontendResults...)

	return printTestSummary(results, discoveryErrs)
}

func runTestIntegration(flags testFlags) error {
	fmt.Println("[test] Running integration tests...")
	fmt.Println()

	var discoveryErrs []error
	results, err := runGoTests("./...", []string{"-tags", "integration"}, flags)
	if err != nil {
		fmt.Fprintf(os.Stderr, "[test] Go integration test discovery failed: %v\n", err)
		discoveryErrs = append(discoveryErrs, fmt.Errorf("integration test discovery: %w", err))
	}
	return printTestSummary(results, discoveryErrs)
}

func runTestE2E(flags testFlags) error {
	fmt.Println("[test] Running e2e tests...")
	fmt.Println()

	if !dirExists("e2e") {
		return fmt.Errorf("no e2e/ directory found")
	}

	// E2E tests always run verbose
	flags.verbose = true

	// Determine E2E test package pattern
	pkg := "./e2e/..."
	if flags.service != "" {
		e2eDir := filepath.Join("e2e", flags.service)
		if !dirExists(e2eDir) {
			return fmt.Errorf("no e2e tests found for service %q (expected %s/)", flags.service, e2eDir)
		}
		pkg = "./" + e2eDir + "/..."
	}

	// Run E2E tests directly — bypass runGoTests service scoping since
	// E2E tests live under e2e/, not handlers/.
	result := runGoTestInDir(".", pkg, nil, flags)
	return printTestSummary([]testResult{result}, nil)
}

// runGoTests runs go test with the given package pattern and optional extra args.
func runGoTests(pkg string, extraArgs []string, flags testFlags) ([]testResult, error) {
	// If --service is specified, scope to that service directory
	if flags.service != "" {
		svcDir := filepath.Join("handlers", flags.service)
		if dirExists(svcDir) && hasGoFiles(svcDir) {
			pkg = "./" + svcDir + "/..."
		} else {
			// Check if it's a frontend — skip Go tests for frontends
			frontendDir := filepath.Join("frontends", flags.service)
			if dirExists(frontendDir) {
				return nil, nil
			}
			return nil, fmt.Errorf("service %q not found under handlers/", flags.service)
		}
	}

	// Always run go test from the project root with the correct package pattern.
	// Each service lives under handlers/<name>/, so we run:
	//   go test ./handlers/<name>/...  for each discovered service.
	serviceDirs := discoverGoServices()
	if len(serviceDirs) == 0 {
		// No service dirs — run from root with the original package pattern
		return []testResult{runGoTestInDir(".", pkg, extraArgs, flags)}, nil
	}

	// If we have a specific package pattern from --service, run from root
	if flags.service != "" {
		return []testResult{runGoTestInDir(".", pkg, extraArgs, flags)}, nil
	}

	var (
		results []testResult
		mu      sync.Mutex
		wg      sync.WaitGroup
	)

	for _, dir := range serviceDirs {
		dir := dir
		// Build the package pattern relative to the project root
		svcPkg := "./" + dir + "/..."
		run := func() {
			result := runGoTestInDir(".", svcPkg, extraArgs, flags)
			mu.Lock()
			results = append(results, result)
			mu.Unlock()
		}

		if flags.parallel && len(serviceDirs) > 1 {
			wg.Add(1)
			go func() {
				defer wg.Done()
				run()
			}()
		} else {
			run()
		}
	}

	wg.Wait()
	return results, nil
}

func runGoTestInDir(dir, pkg string, extraArgs []string, flags testFlags) testResult {
	start := time.Now()
	name := dir
	if name == "." {
		name = "root"
	}

	args := []string{"test"}
	if flags.race {
		args = append(args, "-race")
	}
	if flags.coverage {
		coverFile := filepath.Join(dir, "coverage.out")
		if dir == "." {
			coverFile = "coverage.out"
		}
		args = append(args, "-coverprofile="+coverFile)
	}
	if flags.verbose {
		args = append(args, "-v")
	}
	args = append(args, extraArgs...)
	args = append(args, pkg)

	fmt.Printf("[test] %s: go %s\n", name, strings.Join(args, " "))

	cmd := exec.Command("go", args...)
	cmd.Dir = dir

	output, err := cmd.CombinedOutput()
	duration := time.Since(start)

	passed := err == nil
	if !passed {
		fmt.Fprintf(os.Stderr, "[test] FAIL %s:\n%s\n", name, string(output))
	} else if flags.verbose {
		fmt.Printf("%s\n", string(output))
	}

	return testResult{
		Name:     name,
		Passed:   passed,
		Duration: duration,
		Output:   string(output),
	}
}

// discoverGoServices looks for Go modules or directories with Go files.
// It returns handler service dirs plus the top-level internal/, pkg/, and
// cmd/ trees so that parallel-per-service test runs still cover the whole
// module, not just handlers/.
func discoverGoServices() []string {
	var dirs []string

	// Check handlers/ directory
	if dirExists("handlers") {
		entries, err := os.ReadDir("handlers")
		if err == nil {
			for _, e := range entries {
				if e.IsDir() && e.Name() != "all" && e.Name() != "mocks" {
					svcDir := filepath.Join("handlers", e.Name())
					if hasGoFiles(svcDir) {
						dirs = append(dirs, svcDir)
					}
				}
			}
		}
	}

	// Also include top-level module sub-trees so parallel mode covers
	// internal/, pkg/, and cmd/ packages, not just handlers/.
	for _, top := range []string{"internal", "pkg", "cmd"} {
		if dirExists(top) {
			dirs = append(dirs, top)
		}
	}

	// If no service dirs found, use root if go.mod exists
	if len(dirs) == 0 {
		if _, err := os.Stat("go.mod"); err == nil {
			dirs = append(dirs, ".")
		}
	}

	return dirs
}

func hasGoFiles(dir string) bool {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return false
	}
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".go") {
			return true
		}
	}
	return false
}

// runFrontendTests discovers and runs tests for each Next.js frontend in frontends/.
func runFrontendTests(flags testFlags) []testResult {
	if !dirExists("frontends") {
		return nil
	}

	// If --service is set and it's not a frontend, skip frontend tests
	if flags.service != "" {
		feDir := filepath.Join("frontends", flags.service)
		if !dirExists(feDir) {
			return nil
		}
		// Only test this specific frontend
		return runFrontendTestInDir(flags.service, feDir)
	}

	entries, err := os.ReadDir("frontends")
	if err != nil {
		return nil
	}

	var results []testResult
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		feDir := filepath.Join("frontends", e.Name())
		results = append(results, runFrontendTestInDir(e.Name(), feDir)...)
	}

	return results
}

func runFrontendTestInDir(name, feDir string) []testResult {
	pkgJSON := filepath.Join(feDir, "package.json")
	if _, err := os.Stat(pkgJSON); err != nil {
		return nil
	}

	start := time.Now()
	displayName := fmt.Sprintf("frontends/%s", name)

	fmt.Printf("[test] %s: npm test\n", displayName)

	cmd := exec.Command("npm", "test", "--", "--passWithNoTests")
	cmd.Dir = feDir
	output, runErr := cmd.CombinedOutput()
	duration := time.Since(start)

	passed := runErr == nil
	if !passed {
		fmt.Fprintf(os.Stderr, "[test] FAIL %s:\n%s\n", displayName, string(output))
	}

	return []testResult{{
		Name:     displayName,
		Passed:   passed,
		Duration: duration,
		Output:   string(output),
	}}
}

func printTestSummary(results []testResult, discoveryErrs []error) error {
	fmt.Println()
	fmt.Println("[test] Summary")
	fmt.Println(strings.Repeat("=", 50))

	passed := 0
	failed := 0
	for _, r := range results {
		status := "PASS"
		if !r.Passed {
			status = "FAIL"
			failed++
		} else {
			passed++
		}
		fmt.Printf("  %-4s  %-30s %s\n", status, r.Name, r.Duration.Round(time.Millisecond))
	}

	fmt.Println(strings.Repeat("-", 50))
	fmt.Printf("  Total: %d  Passed: %d  Failed: %d\n", len(results), passed, failed)

	if len(discoveryErrs) > 0 {
		fmt.Println()
		fmt.Fprintln(os.Stderr, "[test] Discovery errors:")
		for _, e := range discoveryErrs {
			fmt.Fprintf(os.Stderr, "  - %v\n", e)
		}
	}

	if failed > 0 {
		if len(discoveryErrs) > 0 {
			return fmt.Errorf("%d test suite(s) failed; %d discovery error(s)", failed, len(discoveryErrs))
		}
		return fmt.Errorf("%d test suite(s) failed", failed)
	}

	if len(discoveryErrs) > 0 {
		return fmt.Errorf("%d test discovery error(s)", len(discoveryErrs))
	}

	fmt.Println()
	fmt.Println("[test] All tests passed.")
	return nil
}