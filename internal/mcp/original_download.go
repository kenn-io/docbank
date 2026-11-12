package mcp

import (
	"context"
	"io"
	"log"
	"maps"
	"math"
	"net/http"
	"net/http/httputil"
	"strings"
	"time"

	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/daemonconn"
)

// OriginalDownloadPath is the HTTP MCP relay route for original capabilities.
const OriginalDownloadPath = "/mcp/original"

const originalDownloadIdleTimeout = 5 * time.Minute

type validatedMCPOriginKey struct{}

var originalDownloadTool = toolDefinition{
	name: "get_original_download_url", title: "Get original download URL",
	description:   "Get a short-lived, single-use URL for the selected original. Download without an API key and verify SHA-256 and size.",
	schemas:       originalDownloadSchemas,
	nonIdempotent: true,
}

type originalDownloadInput struct {
	VaultID          string `json:"vault_id"`
	NodeID           int64  `json:"node_id"`
	ContentVersionID string `json:"content_version_id"`
	BlobHash         string `json:"blob_hash"`
	Size             int64  `json:"size"`
}

type originalDownloadOutput struct {
	privateCache
	originalDownloadInput

	URL              string `json:"url"`
	Path             string `json:"path,omitempty"`
	Name             string `json:"name"`
	ExpiresInSeconds int    `json:"expires_in_seconds"`
}

func getOriginalDownloadURL(ctx context.Context, lease *daemonLease, raw []byte) (originalDownloadOutput, error) {
	var in originalDownloadInput
	if err := decodeReadArguments(raw, &in); err != nil {
		return originalDownloadOutput{}, err
	}
	return daemonProcessingStart(ctx, lease, func(c *daemonconn.Connection) (originalDownloadOutput, error) {
		ticket, err := c.PrepareOriginalDownload(ctx, in.ContentVersionID, api.OriginalDownloadSelection{
			VaultID: in.VaultID, NodeID: in.NodeID, BlobHash: in.BlobHash, Size: in.Size,
		})
		if err != nil {
			return originalDownloadOutput{}, err
		}
		downloadURL := ticket.URL
		var path string
		if origin, ok := ctx.Value(validatedMCPOriginKey{}).(string); ok {
			_, query, _ := strings.Cut(ticket.URL, "?")
			path = OriginalDownloadPath + "?" + query
			downloadURL = origin + path
		}
		return originalDownloadOutput{privateCache: newPrivateCache(), originalDownloadInput: in,
			URL: downloadURL, Path: path, Name: ticket.Name, ExpiresInSeconds: ticket.ExpiresInSeconds}, nil
	})
}

func originalDownloadSchemas() (schema, schema) {
	selection := schema{
		answerVaultIDField: uuidSchema(), nodeField: integerSchema(1, 0), versionField: uuidSchema(),
		"blob_hash": sha256Schema(), sizeField: integerSchema(0, 0),
	}
	required := []string{answerVaultIDField, nodeField, versionField, "blob_hash", sizeField}
	input := rootObjectSchema(selection, required...)
	// Input and output must not share their mutable property map.
	output := maps.Clone(selection)
	output["path"] = stringSchema(4096)
	output["url"] = stringSchema(4096)
	output["name"] = stringSchema(4096)
	output["expires_in_seconds"] = integerSchema(1, math.MaxInt64)
	return input, rootObjectSchema(withPrivateCache(output), cacheRequired(append(required,
		"url", "name", "expires_in_seconds")...)...)
}

// relayOriginalDownload runs after Host/Origin validation, before MCP bearer,
// response bounds, and request deadlines. The ticket is the only authority.
func (server *Server) relayOriginalDownload(w http.ResponseWriter, r *http.Request) {
	ticket := r.URL.Query().Get("ticket")
	proxy := &httputil.ReverseProxy{
		Transport: originalDownloadTransport{lease: server.daemon, ticket: ticket},
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.Out.Header = make(http.Header)
		},
		ErrorLog: log.New(io.Discard, "", 0),
		ErrorHandler: func(w http.ResponseWriter, _ *http.Request, _ error) {
			http.Error(w, "original download unavailable", http.StatusBadGateway)
		},
	}
	proxy.ServeHTTP(w, r)
}

// Every transfer owns its connection. A supplied connection only pins the
// daemon: each download proves and uses a separate connection to it, and the
// supplied one is never held or closed.
type originalDownloadTransport struct {
	lease  *daemonLease
	ticket string
}

func (t originalDownloadTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	ctx, cancel := context.WithCancel(request.Context())
	timer := time.AfterFunc(daemonAcquisitionTimeout, cancel)
	c, err := t.acquire(ctx)
	timer.Stop()
	if err != nil {
		cancel()
		return nil, errDaemonUnavailable
	}
	closeConnection := func() { _ = t.lease.close(c) }
	t.lease.mu.Lock()
	allowed := t.lease.keyPolicy == nil || t.lease.keyPolicy.Allows(c)
	t.lease.mu.Unlock()
	if !allowed {
		cancel()
		closeConnection()
		return nil, errDaemonCredentialReuse
	}
	timer.Reset(originalDownloadIdleTimeout)
	response, err := c.RoundTripOriginalDownload(ctx, t.ticket)
	timer.Stop()
	if err != nil {
		cancel()
		closeConnection()
		return nil, err
	}
	response.Body = &originalDownloadBody{ReadCloser: response.Body, closeConnection: closeConnection, cancel: cancel, timer: timer}
	return response, nil
}

func (t originalDownloadTransport) acquire(ctx context.Context) (*daemonconn.Connection, error) {
	if supplied := t.lease.suppliedConnection; supplied != nil {
		return supplied.TransferConnection(ctx)
	}
	return t.lease.ensure(ctx)
}

type originalDownloadBody struct {
	io.ReadCloser

	closeConnection func()
	cancel          context.CancelFunc
	timer           *time.Timer
}

func (body *originalDownloadBody) Read(p []byte) (int, error) {
	body.timer.Reset(originalDownloadIdleTimeout)
	defer body.timer.Stop()
	return body.ReadCloser.Read(p)
}

func (body *originalDownloadBody) Close() error {
	body.timer.Stop()
	defer body.cancel()
	defer body.closeConnection()
	return body.ReadCloser.Close()
}
