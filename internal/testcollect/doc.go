// Package testcollect pulls Go coverage from an env's running instrumented
// servers into a test run: what `forge test collect <env>` does, and what
// `forge env down` and `forge env preview delete` do before they tear an env
// down.
//
// A server built with `-cover -covermode=atomic` (an instrumented env's Go
// workloads, see internal/cli/build_cover.go) has coverage in two places:
//
//   - live, in the process. serverkit serves it at /debug/coverage on the
//     loopback debug listener (pkg/serverkit/coverage.go): a tar of the
//     covmeta and covcounters files Go would write; a POST carrying
//     X-Forge-Coverage-Reset: 1 zeroes the counters after the tar is built
//     (serverkit refuses a reset in any form a web page could send).
//   - flushed, in GOCOVERDIR. Go writes the counters there when main returns
//     and on os.Exit, so every process that exited cleanly left a file.
//
// Each runtime reaches them its own way:
//
//	host     GET http://127.0.0.1:<pprof port>/debug/coverage, plus the
//	         process's GOCOVERDIR, .forge/tests/covdata/<env>/<workload>/
//	compose  `docker cp <container>:<GOCOVERDIR>/. -`: the scaffolded compose
//	         binds pprof on 0.0.0.0, so serverkit withholds the endpoint, and
//	         a copy needs nothing in the image (a distroless cover stage has
//	         no tar)
//	cluster  a port-forward stream to each running pod's debug port
//	         (pkg/dbquery/kubedial), then the same GET
//
// Files are grouped by their covmeta hash, which names one binary build. Under
// air every rebuild is a new hash, so a workload's GOCOVERDIR can hold several
// builds: the newest is the workload's coverage, and each older one is kept
// beside it, labelled "stale build", and never merged with it (amendment 5:
// Go's own `covdata merge` sums two builds of one package into wrong numbers).
// Different workloads are different binaries with different hashes even from
// one tree; they are kept apart here and merged by pkg/testreport, which
// refuses only different source.
//
// The run layout is pkg/testreport's: the newest build of each workload goes
// in <run>/<lane>/covdata/<workload>/, listed in the lane's capture.json as a
// go-covdata input, and the stale ones in <run>/<lane>/covdata/<workload>/stale/<hash>/,
// listed nowhere, so no reader merges them.
//
// `--reset` zeroes what a run must not count: it sends each live process the
// reset POST and discards the tar, and records the NAMES of the counter files
// flushed before it in .forge/tests/covdata/<env>/.reset/<workload>, which
// every later collect leaves out (fence.go has the whole rule).
//
// Collection is best effort per workload: one that cannot be reached is
// reported, never fatal to the others.
package testcollect
