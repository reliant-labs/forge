package cli

import (
	"fmt"
	"io"
	"net"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// A native (Expo) frontend runs in a simulator, an emulator or on a physical
// device, and "localhost" means a different machine in each:
//
//	iOS simulator     shares the host's loopback        localhost works
//	Android emulator  its own loopback; host is 10.0.2.2  needs 10.0.2.2
//	physical device   the device itself                 needs a LAN address
//
// forge allocates the API's listener (resolveEphemeralHostPorts) and
// ADVERTISES an origin to the frontend; the two are separate decisions. The
// advertised origin stays http://localhost:<port> unless the developer opts in
// to a device-reachable host. forge never picks a network interface for them
// and never starts a tunnel.
//
// Precedence for the native frontend's EXPO_PUBLIC_API_URL, highest first:
//
//  1. an explicit KCL env_vars / config.api_url entry (declared values win
//     over the shell unless the key is named in FORGE_ENV_OVERRIDE);
//  2. the developer's shell value — for a native frontend forge then skips its
//     own injection, though a value in the frontend's .env.<env> file still
//     wins (declared-wins);
//  3. the opt-in device host: --device-host or FORGE_DEV_DEVICE_HOST;
//  4. http://localhost:<port> (unchanged default).
//
// FORGE_DEV_DEVICE_HOST is only read when a native frontend is targeted, so a
// stale value exported globally cannot fail a web-only run.
const (
	deviceHostEnv  = "FORGE_DEV_DEVICE_HOST"
	deviceHostFlag = "device-host"
)

var deviceHostnameRe = regexp.MustCompile(`^[A-Za-z0-9]([A-Za-z0-9-]*[A-Za-z0-9])?(\.[A-Za-z0-9]([A-Za-z0-9-]*[A-Za-z0-9])?)*$`)

// resolveDeviceHost returns the validated, URL-ready device host: the flag
// when set, else FORGE_DEV_DEVICE_HOST, else "" (no opt-in).
func resolveDeviceHost(flagValue string, lookupEnv func(string) (string, bool)) (string, error) {
	raw, source := strings.TrimSpace(flagValue), "--"+deviceHostFlag
	if raw == "" && lookupEnv != nil {
		if v, ok := lookupEnv(deviceHostEnv); ok {
			raw, source = strings.TrimSpace(v), deviceHostEnv
		}
	}
	if raw == "" {
		return "", nil
	}
	host, err := normalizeDeviceHost(raw)
	if err != nil {
		return "", fmt.Errorf("%s: %w", source, err)
	}
	return host, nil
}

// normalizeDeviceHost validates a bare host (hostname, IPv4 or IPv6 literal)
// and returns it in URL-host form (IPv6 bracketed). A scheme, port or path is
// refused: forge owns the scheme and the API's allocated port.
func normalizeDeviceHost(raw string) (string, error) {
	h := strings.TrimSpace(raw)
	switch {
	case strings.Contains(h, "://"):
		return "", fmt.Errorf("%q is a URL; give the host only (e.g. 192.168.1.20) — forge keeps the scheme and the API's allocated port", raw)
	case strings.ContainsAny(h, "/?#@ \t"):
		return "", fmt.Errorf("%q is not a bare host; give a hostname or IP address only", raw)
	}
	bare := strings.TrimSuffix(strings.TrimPrefix(h, "["), "]")
	if ip := net.ParseIP(bare); ip != nil {
		if ip.IsUnspecified() {
			return "", fmt.Errorf("%q is a bind address, not an address a device can reach; use this machine's LAN address", raw)
		}
		if ip.To4() == nil {
			return "[" + ip.String() + "]", nil
		}
		return ip.String(), nil
	}
	if strings.ContainsAny(h, ":[]") {
		return "", fmt.Errorf("%q has a port or malformed IPv6 literal; give the host only — forge keeps the API's allocated port", raw)
	}
	if len(h) > 253 || !deviceHostnameRe.MatchString(h) {
		return "", fmt.Errorf("%q is not a valid hostname or IP address", raw)
	}
	return strings.ToLower(h), nil
}

// isNativeFrontend reports whether the frontend type runs on a device or
// simulator rather than in a browser on this machine.
func isNativeFrontend(frontendType string) bool {
	return frontendEnvPrefix(frontendType) == "EXPO_PUBLIC_"
}

// advertisedAPIOrigin swaps the host of a listener origin for deviceHost,
// keeping scheme and port. An empty deviceHost or origin returns origin as is.
func advertisedAPIOrigin(listenerOrigin, deviceHost string) (string, error) {
	if listenerOrigin == "" || deviceHost == "" {
		return listenerOrigin, nil
	}
	u, err := url.Parse(listenerOrigin)
	if err != nil || u.Host == "" {
		return "", fmt.Errorf("API origin %q is not a URL", listenerOrigin)
	}
	if port := u.Port(); port != "" {
		u.Host = net.JoinHostPort(strings.Trim(deviceHost, "[]"), port)
	} else {
		u.Host = deviceHost
	}
	return u.String(), nil
}

// nativeAPIOrigin is the API origin forge injects into a frontend's launch
// env. Web frontends always get the listener origin (their browser runs on
// this machine); a native frontend gets the device host when one is opted in.
func nativeAPIOrigin(fe FrontendEntity, listenerOrigin, deviceHost string) string {
	if !isNativeFrontend(fe.Type) || deviceHost == "" {
		return listenerOrigin
	}
	origin, err := advertisedAPIOrigin(listenerOrigin, deviceHost)
	if err != nil {
		return listenerOrigin
	}
	return origin
}

// envLookupLast returns the last value assigned to name in an exec-style env.
func envLookupLast(env []string, name string) (string, bool) {
	v, found := "", false
	prefix := name + "="
	for _, kv := range env {
		if strings.HasPrefix(kv, prefix) {
			v, found = kv[len(prefix):], true
		}
	}
	return v, found
}

func envLookupLastOr(env []string, name string) string {
	v, _ := envLookupLast(env, name)
	return v
}

func isLoopbackOrigin(origin string) bool {
	u, err := url.Parse(origin)
	if err != nil {
		return false
	}
	h := u.Hostname()
	if strings.EqualFold(h, "localhost") {
		return true
	}
	ip := net.ParseIP(h)
	return ip != nil && ip.IsLoopback()
}

// describeNativeAPIOrigin returns the launch lines for a native frontend:
// the EFFECTIVE origin (what the app will actually call, after shell and KCL
// overrides) and, when it is a loopback origin, why that only reaches the
// simulator and how to opt in. injected is what forge put in the lowest env
// layer; effective is what the child env finally holds.
func describeNativeAPIOrigin(feName, effective, injected, deviceHost string) []string {
	varName := "EXPO_PUBLIC_API_URL"
	p := "[up] frontend " + feName + ": "
	if effective == "" {
		if deviceHost != "" {
			return []string{p + "--" + deviceHostFlag + " " + deviceHost + " has no effect: no host-mode API origin to advertise (the API is not a host process of this run)"}
		}
		return nil
	}
	var lines []string
	switch {
	case injected != "" && effective != injected:
		src := "set explicitly (shell or KCL env_vars)"
		if deviceHost != "" {
			src += ", overriding --" + deviceHostFlag
		}
		lines = append(lines, fmt.Sprintf("%sAPI origin %s (%s %s)", p, effective, varName, src))
	case deviceHost != "":
		lines = append(lines, fmt.Sprintf("%sAPI origin %s (%s, from --%s / %s)", p, effective, varName, deviceHostFlag, deviceHostEnv))
	default:
		lines = append(lines, fmt.Sprintf("%sAPI origin %s (%s)", p, effective, varName))
	}
	if isLoopbackOrigin(effective) {
		lines = append(lines, p+"localhost reaches the API from the iOS simulator and this machine only; "+
			"an Android emulator needs 10.0.2.2 and a physical device a LAN address or tunnel — "+
			"opt in with --"+deviceHostFlag+" <host> (or "+deviceHostEnv+"=<host>)")
	}
	return lines
}

// deviceHostBindWarning checks, best effort, that the API actually accepts
// connections at the advertised device host. It only judges an IP literal
// that belongs to THIS machine (a LAN address): an emulator alias (10.0.2.2),
// a tunnel or a hostname cannot be dialed meaningfully from here, so those
// are not checked. It returns "" when nothing is wrong or nothing can be
// concluded, never widens a bind, and never fails the run.
func deviceHostBindWarning(deviceHost, apiListenerOrigin string, localAddrs []net.Addr, dial func(addr string, timeout time.Duration) error) string {
	if deviceHost == "" || apiListenerOrigin == "" || dial == nil {
		return ""
	}
	ip := net.ParseIP(strings.Trim(deviceHost, "[]"))
	if ip == nil || !addrsContainIP(localAddrs, ip) {
		return ""
	}
	u, err := url.Parse(apiListenerOrigin)
	if err != nil || u.Port() == "" {
		return ""
	}
	addr := net.JoinHostPort(ip.String(), u.Port())
	if err := dial(addr, 2*time.Second); err == nil {
		return ""
	}
	return fmt.Sprintf("[up] warning: --%s %s is this machine, but the API port %s does not accept connections there — "+
		"the API is probably bound to loopback only, so devices cannot reach it. forge does not widen a bind: "+
		"make the API listen on all interfaces (the scaffolded server binds :%s) or check the firewall",
		deviceHostFlag, deviceHost, u.Port(), u.Port())
}

func addrsContainIP(addrs []net.Addr, ip net.IP) bool {
	for _, a := range addrs {
		switch v := a.(type) {
		case *net.IPNet:
			if v.IP.Equal(ip) {
				return true
			}
		case *net.IPAddr:
			if v.IP.Equal(ip) {
				return true
			}
		}
	}
	return false
}

// dialTCP adapts net.DialTimeout to deviceHostBindWarning's dial seam.
func dialTCP(addr string, timeout time.Duration) error {
	c, err := net.DialTimeout("tcp", addr, timeout)
	if err != nil {
		return err
	}
	return c.Close()
}

// announceNativeAPIOrigin prints the effective API origin of a native
// frontend about to launch. env is the finished launch env, so shell and KCL
// overrides are reflected rather than guessed at.
func announceNativeAPIOrigin(w io.Writer, fl frontendLaunch, fe FrontendEntity, env []string) {
	if !isNativeFrontend(fe.Type) {
		return
	}
	effective, _ := envLookupLast(env, frontendAPIURLEnvVar(fe.Type))
	injected := nativeAPIOrigin(fe, fl.apiBaseURL, fl.deviceHost)
	for _, line := range describeNativeAPIOrigin(fe.Name, effective, injected, fl.deviceHost) {
		fmt.Fprintln(w, line)
	}
	port, _ := strconv.Atoi(envLookupLastOr(env, "PORT"))
	for _, line := range describeNativeMetroPort(fe.Name, port) {
		fmt.Fprintln(w, line)
	}
}

// warnDeviceHostBinding warns when the opted-in device host is this machine
// but the API refuses connections on it. Only runs when a native frontend is
// part of the run.
// targetsNativeFrontend reports whether this run launches a native frontend.
func targetsNativeFrontend(e *KCLEntities, targets []string) bool {
	if e == nil {
		return false
	}
	for _, fe := range e.Frontends {
		if inTargetSet(targets, fe.Name) && isNativeFrontend(fe.Type) {
			return true
		}
	}
	return false
}

func warnDeviceHostBinding(w io.Writer, e *KCLEntities, targets []string, deviceHost, apiBaseURL string) {
	if deviceHost == "" || !targetsNativeFrontend(e, targets) {
		return
	}
	addrs, _ := net.InterfaceAddrs()
	if msg := deviceHostBindWarning(deviceHost, apiBaseURL, addrs, dialTCP); msg != "" {
		fmt.Fprintln(w, msg)
	}
}
