package serverkit

// rest.go — REST URLs over the mounted Connect services (forge.yaml `api.rest`).
//
// vanguard v0.5 serves REST from a connect-go v2 *connect.Server, and forge's
// handlers are connect-go v1 http.Handlers on a mux. The bridge is vanguard's
// own proxy shape: every mounted service is registered on a v2 server as
// vanguard.ForwardService over a Connect client whose transport serves each
// call with the mux, in process. A REST request is decoded by vanguard,
// relayed as a Connect call, and answered by the very handler — interceptor
// chain, auth and all — that answers the Connect URL. Connect, gRPC and
// gRPC-Web requests never touch vanguard.

import (
	"fmt"
	"log/slog"
	"net/http"
	"strings"

	connectv2 "connectrpc.com/connect/v2"
	"connectrpc.com/connect/v2/connecthttp"
	"connectrpc.com/vanguard"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/reflect/protoregistry"

	"github.com/reliant-labs/forge/pkg/internal/inprocess"
)

// RESTHandler serves the REST URLs that the MOUNTED services' google.api.http
// annotations declare, in front of mux. A request mux has a route for —
// every Connect procedure, and anything else registered on it — goes to mux
// unchanged; any other request is matched against the REST routes, and one
// that matches none gets vanguard's 404.
//
// It returns nil when nothing was mounted, and nil when the REST routes
// cannot be built — in both cases the caller keeps serving the bare mux
// rather than serving nothing. That is the deliberate choice: losing REST
// URLs is a degradation, losing Connect is an outage, and REST routes that
// failed to build are not evidence the Connect handlers are broken.
//
// connectPaths are the fully-qualified Connect service names a mount
// returned — the same values the completeness gate records. A path form
// ("/pkg.v1.Service/") is accepted too.
func RESTHandler(mux *http.ServeMux, connectPaths []string, logger *slog.Logger) http.Handler {
	if len(connectPaths) == 0 {
		return nil
	}
	rest, err := newRESTHandler(mux, connectPaths)
	if err != nil {
		if logger == nil {
			logger = slog.Default()
		}
		logger.Error("init vanguard REST handler", "error", err)
		return nil
	}
	return rest
}

func newRESTHandler(mux *http.ServeMux, connectPaths []string) (http.Handler, error) {
	upstream := connectv2.NewClient(connecthttp.NewTransport(
		&http.Client{Transport: inprocess.Transport{Handler: mux}}, inprocess.BaseURL))
	server := connectv2.NewServer()
	for _, p := range connectPaths {
		name := strings.Trim(p, "/")
		desc, err := protoregistry.GlobalFiles.FindDescriptorByName(protoreflect.FullName(name))
		if err != nil {
			return nil, fmt.Errorf("resolve the schema of service %q: %w", p, err)
		}
		svc, ok := desc.(protoreflect.ServiceDescriptor)
		if !ok {
			return nil, fmt.Errorf("resolve the schema of service %q: it names a %T, not a service", p, desc)
		}
		server.Register(vanguard.ForwardService(upstream, svc)...)
	}
	routes := http.NewServeMux()
	if err := vanguard.Mount(routes, server); err != nil {
		return nil, err
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, pattern := mux.Handler(r); pattern != "" {
			mux.ServeHTTP(w, r)
			return
		}
		routes.ServeHTTP(w, r.WithContext(inprocess.WithOrigin(r.Context(), r)))
	}), nil
}
