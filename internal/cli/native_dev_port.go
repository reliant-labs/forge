package cli

import (
	"fmt"
	"io"
	"net"
	"sort"
	"strconv"
	"strings"

	"github.com/reliant-labs/forge/internal/config"
)

// expoMetroDefaultPort is the port a native (Expo / React Native) Debug build
// looks for Metro on unless told otherwise:
//
//	iOS      RCTBundleURLProvider builds "http://<host>:RCT_METRO_PORT/" for a
//	         host with no explicit port; RCT_METRO_PORT defaults to 8081 in
//	         React/Base/RCTDefines.h and is set per build through the
//	         RCT_METRO_PORT build setting (React-Core.podspec forwards it as a
//	         preprocessor define).
//	Android  the react_native_dev_server_port resource (8081) read by
//	         AndroidInfoHelpers.
//
// (react-native v0.88.1, packages/react-native.) A forge-allocated ephemeral
// Metro port is therefore invisible to such a build — "No script URL
// provided" — so a native frontend prefers this port whenever it is free.
const expoMetroDefaultPort = 8081

// tcpPortFree reports whether nothing on this host holds the port: no
// listener answers on loopback and the wildcard address can be bound. The
// bind probe is released at once, the same reserve-then-release window
// freeTCPPort has for the ephemeral ports.
func tcpPortFree(port int) bool {
	if portInUse(port) {
		return false
	}
	l, err := net.Listen("tcp", ":"+strconv.Itoa(port))
	if err != nil {
		return false
	}
	_ = l.Close()
	return true
}

// forgeReclaimablePortFree is the probe `forge env up` uses for the preferred
// Metro port: free per tcpPortFree, or held by this (project, env) stack's own
// process for the SAME frontend. upPreflight stops that predecessor before
// launch, so re-running `forge env up` keeps 8081 instead of flipping to an
// ephemeral port every other run. A holder forge does not own, or one owned
// by a different service this run will not restart, is never reclaimed: the
// port is reported busy.
func forgeReclaimablePortFree(projectID, env string) func(feName string, port int) bool {
	return func(feName string, port int) bool {
		if tcpPortFree(port) {
			return true
		}
		pid := portListenerPID(port)
		if pid <= 0 {
			return false
		}
		svc, owned := forgeOwnerOfPID(pid, projectID, env, newOSProcFacts())
		return owned && strings.TrimPrefix(svc, "frontend:") == feName
	}
}

// pickNativeMetroPort returns Expo's conventional port when nothing else this
// run allocates has claimed it and free reports it available, else 0 (the
// caller falls back to an ephemeral port).
func pickNativeMetroPort(feName string, claimed map[int]bool, free func(feName string, port int) bool) int {
	if claimed[expoMetroDefaultPort] || free == nil || !free(feName, expoMetroDefaultPort) {
		return 0
	}
	return expoMetroDefaultPort
}

// metroPreference tracks the native frontends whose Metro port is only a
// PREFERENCE (Expo's 8081, picked because it was free when the run resolved
// ports), as opposed to a port the KCL declared. A preferred port can be taken
// by a foreign process before Metro binds it, so it is re-checked just ahead of
// the preflight port scan and again just before launch; a declared port is
// never re-checked here and keeps its fail-loudly behavior.
type metroPreference struct {
	cfg *config.ProjectConfig
	// names holds the frontends still on their preferred port.
	names map[string]bool
	// alloc reserves a fresh ephemeral port; nil means freeTCPPort.
	alloc func() (int, error)
}

// settle moves a still-preferred frontend off its port when free says the port
// is held by a process forge does not own, switching it to an ephemeral port in both the entity
// set and cfg. The holder is never touched and the run never fails here.
// Reports the port the frontend ends up on, or 0 when fe was not preferred.
func (m *metroPreference) settle(e *KCLEntities, feName string, free func(feName string, port int) bool, w io.Writer) int {
	if m == nil || e == nil || !m.names[feName] {
		return 0
	}
	for i := range e.Frontends {
		fe := &e.Frontends[i]
		if fe.Name != feName {
			continue
		}
		if free == nil || free(feName, fe.Port) {
			return fe.Port
		}
		alloc := m.alloc
		if alloc == nil {
			alloc = freeTCPPort
		}
		port, err := alloc()
		if err != nil {
			fmt.Fprintf(w, "[up] frontend %s: preferred Metro port %d is held by another process and no ephemeral port could be allocated (%v)\n", feName, fe.Port, err)
			return fe.Port
		}
		fmt.Fprintf(w, "[up] frontend %s: preferred Metro port %d is held by a process forge doesn't manage (left running); using ephemeral port %d\n", feName, fe.Port, port)
		fe.Port = port
		delete(m.names, feName)
		if m.cfg != nil {
			for j := range m.cfg.Frontends {
				if m.cfg.Frontends[j].Name == feName {
					m.cfg.Frontends[j].Port = port
				}
			}
		}
		return port
	}
	return 0
}

// metroPortFreeAtLaunch is the probe used just before Metro starts. Every
// predecessor of this run was stopped by upPreflight, so any holder still on
// the port is not wanted there and the frontend moves instead.
func metroPortFreeAtLaunch(_ string, port int) bool { return tcpPortFree(port) }

// settleAll runs settle for every frontend still on a preferred port.
func (m *metroPreference) settleAll(e *KCLEntities, free func(feName string, port int) bool, w io.Writer) {
	if m == nil {
		return
	}
	names := make([]string, 0, len(m.names))
	for n := range m.names {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		m.settle(e, n, free, w)
	}
}

// claimedFrontendPorts is every port this run already declared or allocated
// for a frontend or host workload, so a native frontend never takes 8081 from
// a service that binds it.
func claimedFrontendPorts(e *KCLEntities) map[int]bool {
	claimed := map[int]bool{}
	for _, fe := range e.Frontends {
		if fe.Port > 0 {
			claimed[fe.Port] = true
		}
	}
	for _, w := range e.WorkloadsOn(RuntimeHost) {
		for _, p := range w.HostPorts() {
			claimed[p] = true
		}
	}
	return claimed
}

// devlogOriginEnv names the env var the native dev-log forwarder reads to find
// Metro's POST /__forge/log endpoint. A plain Debug build (no expo-dev-client)
// exposes no hostUri/experienceUrl/linkingUri through expo-constants, so forge,
// which knows Metro's port and the device host, hands it over. Expo inlines
// EXPO_PUBLIC_* into the bundle at bundle time, and Metro runs with this env.
const devlogOriginEnv = "EXPO_PUBLIC_FORGE_DEVLOG_ORIGIN"

// nativeDevlogOrigin is http://<deviceHost or localhost>:<metroPort> for a
// native frontend, using the same device host as the API origin, and "" for a
// web frontend or one with no known port.
func nativeDevlogOrigin(fe FrontendEntity, deviceHost string) string {
	if !isNativeFrontend(fe.Type) || fe.Port <= 0 {
		return ""
	}
	host := strings.Trim(deviceHost, "[]")
	if host == "" {
		host = "localhost"
	}
	return "http://" + net.JoinHostPort(host, strconv.Itoa(fe.Port))
}

// describeNativeMetroPort is the single launch line for a native frontend
// whose Metro is NOT on Expo's default port, saying how to point a build at
// it. Nothing is printed on 8081 (builds find it unaided) or when no port
// was allocated. With a device host the target is a physical device (or an
// emulator reaching this machine): the simulator's `simctl launch ... localhost`
// would reach nothing, so the line names the rebuild and the one-time Local
// Network prompt instead.
func describeNativeMetroPort(feName string, port int, deviceHost string) []string {
	if port <= 0 || port == expoMetroDefaultPort {
		return nil
	}
	if deviceHost != "" {
		return []string{fmt.Sprintf("[up] frontend %s: Metro is on :%d, not %d where a native build looks by default — "+
			"on a physical device rebuild with RCT_METRO_PORT=%d so the build finds it at %s:%d; "+
			"the first launch also needs \"Local Network\" allowed on the device (accept the prompt, then relaunch: React Native shows `No script URL provided` until you do); "+
			"Android emulator: `adb reverse tcp:%d tcp:%d`, then Dev Menu > Change Bundle Location",
			feName, port, expoMetroDefaultPort, port, strings.Trim(deviceHost, "[]"), port, port, port)}
	}
	return []string{fmt.Sprintf("[up] frontend %s: Metro is on :%d, not %d where a native build looks by default — "+
		"iOS: `xcrun simctl launch <udid> <bundle-id> -RCT_jsLocation localhost:%d` (or rebuild with RCT_METRO_PORT=%d); "+
		"Android: `adb reverse tcp:%d tcp:%d`, then Dev Menu > Change Bundle Location > localhost:%d; "+
		"dev client: open the URL Metro prints",
		feName, port, expoMetroDefaultPort, port, port, port, port, port)}
}
