package serverkit

// rest.go — the REST-transcoder wrap (forge.yaml `api.rest`).

import (
	"log/slog"
	"net/http"

	"connectrpc.com/vanguard"
)

// RESTHandler wraps a Connect mux in a vanguard transcoder over the MOUNTED
// services' Connect paths, so the same handlers answer REST-shaped URLs in
// addition to Connect ones.
//
// It returns nil when nothing was mounted, and nil when the transcoder
// cannot be built — in both cases the caller keeps serving the bare mux
// rather than serving nothing. That is the deliberate choice: losing REST
// URLs is a degradation, losing Connect is an outage, and a transcoder that
// failed to build is not evidence the Connect handlers are broken.
//
// connectPaths are the fully-qualified Connect service names a mount
// returned — the same values the completeness gate records.
func RESTHandler(mux *http.ServeMux, connectPaths []string, logger *slog.Logger) http.Handler {
	if len(connectPaths) == 0 {
		return nil
	}
	svcs := make([]*vanguard.Service, 0, len(connectPaths))
	for _, p := range connectPaths {
		svcs = append(svcs, vanguard.NewService(p, mux))
	}
	transcoder, err := vanguard.NewTranscoder(svcs)
	if err != nil {
		if logger == nil {
			logger = slog.Default()
		}
		logger.Error("init vanguard REST transcoder", "error", err)
		return nil
	}
	return transcoder
}
