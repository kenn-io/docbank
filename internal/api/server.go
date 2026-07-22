package api

import (
	"context"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"log/slog"
	"net/http"
	"os"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"github.com/danielgtaylor/huma/v2/adapters/humago"
	kitdaemon "go.kenn.io/kit/daemon"

	"go.kenn.io/docbank/internal/blob"
	"go.kenn.io/docbank/internal/config"
	"go.kenn.io/docbank/internal/daemonauth"
	"go.kenn.io/docbank/internal/jobs"
	internalmaintenance "go.kenn.io/docbank/internal/maintenance"
	"go.kenn.io/docbank/internal/store"
	"go.kenn.io/docbank/internal/version"
)

const kitPingPath = kitdaemon.DefaultPingPath

// VerifyPageFunc performs one bounded content-verification page. The optional
// dependency lets embedders test legacy route error reporting at this boundary.
type VerifyPageFunc func(
	context.Context, *store.Store, *blob.Store, internalmaintenance.VerifyOptions,
) (internalmaintenance.VerifyReport, error)

// RepackPageFunc performs one bounded packed-maintenance page. The optional
// dependency lets embedders exercise legacy route continuation behavior.
type RepackPageFunc func(
	context.Context, *store.Store, *blob.Store, internalmaintenance.RepackOptions,
) (internalmaintenance.RepackReport, error)

// Deps assembles everything a Server needs to build its routes.
type Deps struct {
	Store         *store.Store
	Blobs         *blob.Store
	VaultRoot     string // live vault root; backup restore must remain disjoint
	Cfg           config.Config
	Logger        *slog.Logger // nil → slog.Default()
	StartedAt     time.Time
	ShutdownToken string           // "" disables the shutdown route
	Shutdown      func()           // called (async) by the shutdown route
	Tracker       *ActivityTracker // nil → no idle tracking
	Jobs          *jobs.Supervisor // nil → no registered background jobs
	Gate          *OperationGate   // nil → a server-private gate
	VerifyPage    VerifyPageFunc   // nil → shared bounded maintenance service
	RepackPage    RepackPageFunc   // nil → shared bounded maintenance service
}

// Server is docbank's HTTP API: a huma-described /api/v1 surface plus a
// handful of plain http.Handler routes (health, ping, shutdown, web).
type Server struct {
	deps          Deps
	handler       http.Handler
	api           huma.API
	auditPreviews *auditPreviewRegistry
}

// NewServer wires all routes and middleware onto a fresh mux. The handler
// is safe to mount under httptest; nothing here binds a socket.
func NewServer(d Deps) *Server {
	if d.Cfg.Server.APIKey == "" {
		// A serving daemon always has an effective key (configured or
		// ephemeral; see cmd/docbank/daemon.go). An empty key here means
		// a caller forgot to set one — the one sanctioned exception is
		// OpenAPIYAML's offline document render, which supplies a
		// placeholder key precisely to avoid tripping this check.
		panic("api: NewServer requires a non-empty Cfg.Server.APIKey")
	}
	if d.Store != nil && d.VaultRoot == "" {
		panic("api: NewServer requires VaultRoot when serving a store")
	}
	installErrorFormatter()
	if d.Logger == nil {
		d.Logger = slog.Default()
	}
	if d.StartedAt.IsZero() {
		d.StartedAt = time.Now()
	}

	mux := http.NewServeMux()
	cfg := huma.DefaultConfig("docbank", version.Version)
	// Every /api/v1 operation sits behind authMiddleware; the document-level
	// security requirement tells generated clients that credentials are
	// mandatory (either header form works, see the middleware).
	cfg.Components.SecuritySchemes = map[string]*huma.SecurityScheme{
		"apiKey": {Type: "apiKey", In: "header", Name: "X-Api-Key",
			Description: "The daemon's API key: configured `api_key`, or the " +
				"ephemeral per-run key published in the runtime record."},
		"bearer": {Type: "http", Scheme: "bearer",
			Description: "The same API key as a bearer token."},
	}
	cfg.Security = []map[string][]string{{"apiKey": {}}, {"bearer": {}}}
	humaAPI := humago.New(mux, cfg)
	s := &Server{deps: d, api: humaAPI, auditPreviews: newAuditPreviewRegistry()}
	g := d.Gate
	if g == nil {
		g = NewOperationGate()
	}

	registerReadRoutes(humaAPI, d) // Task 5 (stat-by-id lands in this task)
	registerInfoRoute(humaAPI, d)
	registerMutateRoutes(humaAPI, d, g) // Task 6
	registerOpsRoutes(humaAPI, d, g)    // Task 7
	registerBackupRoutes(humaAPI, d, g)
	registerJobRoutes(humaAPI, d)
	registerWatchRoutes(humaAPI, d)
	registerUploadRoute(mux, humaAPI, d, g)
	registerContentWriteRoute(mux, humaAPI, d, g)
	registerContentRevertRoute(humaAPI, d, g)
	registerContentPruneRoute(humaAPI, d, g)
	registerProvenanceRoutes(humaAPI, d)
	registerTagRoutes(humaAPI, d, g)
	registerAuditRoutes(humaAPI, d, g, s.auditPreviews)
	clearLongRunningBodyReadDeadlines(humaAPI)
	markRevisionPreconditionsRequired(humaAPI)
	s.registerHealth(mux)
	mux.Handle("GET "+kitPingPath, kitdaemon.NewPingHandler(kitdaemon.PingHandlerOptions{
		Service: "docbank", Version: version.Version, PID: os.Getpid(),
	}))
	s.registerChallenge(mux)
	s.registerShutdown(mux)
	registerWeb(mux, d.Cfg.Web.Enabled)

	h := http.Handler(mux)
	// Authenticate and enforce route topology before buffering small JSON
	// envelopes, then validate their raw text before Huma's encoding/json
	// decoder can perform lossy Unicode replacement.
	h = jsonBodyTextMiddleware(h)
	h = authMiddleware(h, d.Cfg.Server.APIKey)
	h = loopbackMiddleware(h)
	h = timeoutMiddleware(h)
	h = recoverMiddleware(h, d.Logger)
	h = logMiddleware(h, d.Logger)
	h = trackMiddleware(h, d.Tracker)
	s.handler = h
	return s
}

func (s *Server) Handler() http.Handler { return s.handler }
func (s *Server) API() huma.API         { return s.api }

// markRevisionPreconditionsRequired keeps Huma's runtime parser permissive
// enough for parseIfMatch to return Docbank's structured 428 response while
// making the actual wire requirement unambiguous to generated clients.
func markRevisionPreconditionsRequired(api huma.API) {
	for _, route := range []struct{ path, method string }{
		{"/api/v1/nodes/{id}", http.MethodPatch},
		{"/api/v1/nodes/{id}/trash", http.MethodPost},
		{"/api/v1/nodes/{id}/restore", http.MethodPost},
		{"/api/v1/nodes/{id}/verify", http.MethodPost},
		{"/api/v1/nodes/{id}/revert", http.MethodPost},
		{"/api/v1/nodes/{id}/versions/prune", http.MethodPost},
		{"/api/v1/nodes/{id}/tags/{tag_id}", http.MethodPut},
		{"/api/v1/nodes/{id}/tags/{tag_id}", http.MethodDelete},
		{"/api/v1/tags/{tag_id}", http.MethodPatch},
		{"/api/v1/tags/{tag_id}", http.MethodDelete},
	} {
		markDocumentedHeaderRequired(api, route.path, route.method, "If-Match")
	}
}

func markDocumentedHeaderRequired(api huma.API, path, method, header string) {
	item := api.OpenAPI().Paths[path]
	var operation *huma.Operation
	switch method {
	case http.MethodPost:
		operation = item.Post
	case http.MethodPut:
		operation = item.Put
	case http.MethodPatch:
		operation = item.Patch
	case http.MethodDelete:
		operation = item.Delete
	default:
		panic("unsupported documented header method " + method)
	}
	for index, parameter := range operation.Parameters {
		if parameter.In == "header" && parameter.Name == header {
			documentedParameter := *parameter
			documentedParameter.Required = true
			operation.Parameters[index] = &documentedParameter
			return
		}
	}
	panic("route lacks documented header " + header)
}

func (s *Server) registerHealth(mux *http.ServeMux) {
	type health struct {
		Status        string `json:"status"`
		Version       string `json:"version"`
		UptimeSeconds int64  `json:"uptime_seconds"`
	}
	mux.HandleFunc("GET /health", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, health{
			Status: "ok", Version: version.Version,
			UptimeSeconds: int64(time.Since(s.deps.StartedAt).Seconds()),
		})
	})
}

// registerChallenge proves that the process answering public ping owns the
// owner-private runtime record. The token never crosses the socket: the client
// supplies a fresh nonce and verifies this HMAC before sending API credentials.
func (s *Server) registerChallenge(mux *http.ServeMux) {
	if s.deps.ShutdownToken == "" {
		return
	}
	mux.HandleFunc("GET "+daemonauth.ChallengePath, func(w http.ResponseWriter, r *http.Request) {
		nonce, err := hex.DecodeString(r.URL.Query().Get("nonce"))
		if err != nil || len(nonce) != daemonauth.NonceBytes {
			writeError(w, NewError(http.StatusBadRequest, "invalid_challenge",
				"nonce must be 32 bytes encoded as hexadecimal"))
			return
		}
		writeJSON(w, http.StatusOK, struct {
			Proof string `json:"proof"`
		}{Proof: daemonauth.Proof(s.deps.ShutdownToken, nonce)})
	})
}

// registerShutdown adds the hidden token-gated endpoint daemon stop uses.
// Not in the OpenAPI document: it is lifecycle plumbing, not agent surface.
func (s *Server) registerShutdown(mux *http.ServeMux) {
	if s.deps.ShutdownToken == "" {
		return
	}
	mux.HandleFunc("POST /api/daemon/shutdown", func(w http.ResponseWriter, r *http.Request) {
		got := r.Header.Get("X-Docbank-Daemon-Token")
		if subtle.ConstantTimeCompare([]byte(got), []byte(s.deps.ShutdownToken)) != 1 {
			writeError(w, NewError(http.StatusUnauthorized, "unauthorized", "bad shutdown token"))
			return
		}
		w.WriteHeader(http.StatusAccepted)
		if s.deps.Shutdown != nil {
			go s.deps.Shutdown()
		}
	})
}

// writeJSON encodes v as the response body with the given status code.
// Status and headers are already committed by the time Encode runs; every
// caller in this package passes a fixed, JSON-safe struct, so there is
// nothing actionable to do if encoding somehow fails.
func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v) //nolint:errchkjson // see above
}
