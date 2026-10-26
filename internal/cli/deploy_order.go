package cli

// THE ORDER ONE DEPLOY APPLIES ITS PLACEMENTS IN, derived from declared
// dependencies — never a fixed tier (docs/design/per-workload-placement.md §6).
//
// A `forge env deploy` of an env that mixes placements runs three stages:
//
//   - local     the parts forge applies from this machine: cluster workloads,
//     cluster databases, compose services, host infra
//   - hosted    the parts published to the control plane: OnHosted workloads,
//     hosted databases, OnHosted frontends (StaticSites)
//   - frontends the frontends forge builds and ships itself: OnBucket,
//     OnFirebase, BuildOnly
//
// Each stage waits for what it applied before the next starts (the local apply
// waits on rollout, the hosted publish on the platform's readiness), so a
// stage that runs after another sees it Ready at this release.
//
// THE EDGES ARE DECLARATIONS:
//
//   - `release_dependencies = {peer: contract}` on a workload, frontend or
//     database says "I consume peer's contract", so peer goes first. It is the
//     claim the release-cohort machinery already reads
//     (release_compose_declaration.go); one declaration answers both "must
//     change together" and "goes first".
//   - A FRONTEND whose release_dependencies is absent — which that field
//     defines as UNKNOWN — waits for every server workload of the release,
//     whatever its placement. Explicit {} means independent. A workload with
//     it absent gets no edge, which is the order forge always had.
//
// Edges inside one stage are that stage's business (the platform's converger
// orders the hosted parts, Flux the delivery units). With no edge at all, the
// order is local → hosted → frontends: what forge always did. A cycle between
// stages is refused, naming one edge each way, before anything ships.

import (
	"fmt"
	"sort"
	"strings"
)

// deployStage is one of the three stages above.
type deployStage string

const (
	stageLocal     deployStage = "local"
	stageHosted    deployStage = "hosted"
	stageFrontends deployStage = "frontends"
)

// defaultStageOrder is the order with no edges, and the tie-break.
var defaultStageOrder = []deployStage{stageLocal, stageHosted, stageFrontends}

// stageEdge is one declared dependency that crosses stages: Dependent's stage
// runs after Dependency's.
type stageEdge struct {
	Dependency, Dependent string
	From, To              deployStage
}

// stageOfWorkload is the stage a workload deploys in; "" when no deploy
// applies it (build-only, a host process `forge env up` runs).
func stageOfWorkload(w WorkloadEntity) deployStage {
	switch w.Runtime.Type {
	case RuntimeHosted:
		return stageHosted
	case RuntimeCluster, RuntimeCompose:
		return stageLocal
	}
	return ""
}

func stageOfFrontend(f FrontendEntity) deployStage {
	switch {
	case frontendIsHosted(f):
		return stageHosted
	case f.Runtime.Ships() || f.Runtime.Type == FrontendRuntimeBuildOnly:
		return stageFrontends
	}
	return ""
}

func stageOfDatabase(d DatabaseEntity) deployStage {
	if d.Hosted() {
		return stageHosted
	}
	return stageLocal
}

// deployStageEdges lists every declared dependency that crosses stages. Pure;
// sorted, so a refusal names the same edge on every run.
func deployStageEdges(e *KCLEntities) []stageEdge {
	if e == nil {
		return nil
	}
	stageOf := map[string]deployStage{}
	var servers []string
	for _, w := range e.Workloads {
		if s := stageOfWorkload(w); s != "" {
			stageOf[w.Name] = s
			servers = append(servers, w.Name)
		}
	}
	for _, d := range e.Databases {
		if _, taken := stageOf[d.Name]; !taken {
			stageOf[d.Name] = stageOfDatabase(d)
		}
	}
	var edges []stageEdge
	add := func(dependent string, to deployStage, peers []string) {
		for _, peer := range peers {
			from, ok := stageOf[peer]
			if !ok || from == to || peer == dependent {
				continue
			}
			edges = append(edges, stageEdge{Dependency: peer, Dependent: dependent, From: from, To: to})
		}
	}
	for _, w := range e.Workloads {
		if s := stageOfWorkload(w); s != "" {
			add(w.Name, s, sortedKeys(w.ReleaseDependencies))
		}
	}
	for _, f := range e.Frontends {
		s := stageOfFrontend(f)
		if s == "" {
			continue
		}
		if f.ReleaseDependencies == nil {
			add(f.Name, s, servers)
		} else {
			add(f.Name, s, sortedKeys(f.ReleaseDependencies))
		}
	}
	sort.Slice(edges, func(i, j int) bool {
		a, b := edges[i], edges[j]
		if a.Dependent != b.Dependent {
			return a.Dependent < b.Dependent
		}
		return a.Dependency < b.Dependency
	})
	return edges
}

// deployStageOrder is the order the stages run in: a topological order of the
// cross-stage edges, ties broken by defaultStageOrder.
func deployStageOrder(e *KCLEntities) ([]deployStage, error) {
	edges := deployStageEdges(e)
	after := map[deployStage]map[deployStage]stageEdge{} // to -> from -> a witness
	for _, ed := range edges {
		if after[ed.To] == nil {
			after[ed.To] = map[deployStage]stageEdge{}
		}
		if _, seen := after[ed.To][ed.From]; !seen {
			after[ed.To][ed.From] = ed
		}
	}
	done := map[deployStage]bool{}
	var order []deployStage
	for len(order) < len(defaultStageOrder) {
		progressed := false
		for _, s := range defaultStageOrder {
			if done[s] {
				continue
			}
			ready := true
			for from := range after[s] {
				if !done[from] {
					ready = false
					break
				}
			}
			if ready {
				done[s], order, progressed = true, append(order, s), true
				break
			}
		}
		if !progressed {
			return nil, stageCycleError(after, done)
		}
	}
	return order, nil
}

// stageCycleError names one edge of the cycle each way.
func stageCycleError(after map[deployStage]map[deployStage]stageEdge, done map[deployStage]bool) error {
	var parts []string
	for _, to := range defaultStageOrder {
		if done[to] {
			continue
		}
		for _, from := range defaultStageOrder {
			if ed, ok := after[to][from]; ok && !done[from] {
				parts = append(parts, fmt.Sprintf("%s (%s) needs %s (%s)", ed.Dependent, ed.To, ed.Dependency, ed.From))
			}
		}
	}
	return fmt.Errorf("the env's declared release_dependencies order its placements in a cycle: %s.\n"+
		"One deploy applies the parts forge applies, then the hosted parts, then the frontends forge ships, in whatever order "+
		"those dependencies give, so a hosted part and a cluster part cannot each wait for the other.\n"+
		"fix: drop one of those dependencies, or move one part to the other's placement",
		strings.Join(parts, "; "))
}
