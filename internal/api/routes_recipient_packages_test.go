package api_test

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json/v2"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"
	"uuid"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/document/pagerender"
	"go.kenn.io/docbank/document/redaction"
	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/processing"
	"go.kenn.io/docbank/internal/redactiontest"
	"go.kenn.io/docbank/internal/store"
)

type recipientHTTPFixture struct {
	vault   *testStore
	key     string
	service *processing.Service
	request processing.RedactionRenderRequest
	runtime *pagerender.Runtime
	spool   string
}

func newRecipientHTTPFixture(t *testing.T, gate *pausedCitationGate) recipientHTTPFixture {
	t.Helper()
	ctx := t.Context()
	runtime := redactiontest.PageRuntime(t)
	var service *processing.Service
	var spool string
	vault, key := newTestAPIServer(t, func(d *api.Deps) {
		var err error
		spool = filepath.Join(d.VaultRoot, "blobs", "tmp")
		config := processing.ServiceConfig{
			Catalog: d.Store, Blobs: d.Blobs, Gate: api.NewOperationGate(),
			SpoolDirectory: spool,
		}
		if gate != nil {
			config.Gate = gate
		}
		service, err = processing.NewService(ctx, config)
		require.NoError(t, err)
		t.Cleanup(func() { require.NoError(t, service.Shutdown(context.Background())) })
		d.Processing = service // No page runtime is configured on the HTTP server.
	})
	pdf := redactiontest.PDF(t, []string{"SECRET KEEP SECRET"}, "")
	hash, size, err := vault.Blobs.WriteContext(ctx, bytes.NewReader(pdf))
	require.NoError(t, err)
	node, err := vault.CreateFile(ctx, vault.RootID(), "source.pdf", hash, size, "application/pdf")
	require.NoError(t, err)
	prep, err := service.PrepareNativePDF(ctx, processing.RedactionPreparationRequest{
		Selection: store.PageBinding{NodeID: node.ID, Revision: node.Revision,
			Source: document.PageSource{VersionID: node.CurrentVersionID, SHA256: hash, Size: size}},
	}, runtime)
	require.NoError(t, err)
	masks := []processing.RedactionMask{{ID: uuid.New(), Selector: redaction.Selector{
		Kind: "text", MapSHA256: prep.Map.SHA256, Span: &redaction.Span{End: 6},
	}}}
	review, err := service.ResolveNativePDF(ctx, processing.RedactionResolveRequest{Preparation: prep, Masks: masks}, runtime)
	require.NoError(t, err)
	ns, err := vault.EnsureBatesNamespace(ctx, "HANDOFF", "", 6)
	require.NoError(t, err)
	r, err := service.ReserveNumberedPDF(ctx, processing.NumberedReservationRequest{
		Preparation: prep, Masks: masks, ReviewSHA256: review.SHA256,
		NamespaceID: uuid.MustParse(ns.NamespaceID), StartAt: 1, RetainOriginal: true,
	}, runtime)
	require.NoError(t, err)
	request := processing.RedactionRenderRequest{Preparation: prep, Masks: masks,
		ReviewSHA256: review.SHA256, AllocationID: r.AllocationID}
	return recipientHTTPFixture{vault: vault, key: key, service: service, request: request, runtime: runtime, spool: spool}
}

//nolint:paralleltest // Qualified PDFium measures whole-process RSS.
func TestRecipientPackageHTTP(t *testing.T) {
	ctx := t.Context()
	resume := make(chan struct{})
	close(resume)
	gate := &pausedCitationGate{OperationGate: api.NewOperationGate(), entered: make(chan struct{}), resume: resume}
	f := newRecipientHTTPFixture(t, gate)
	service, runtime, request, spool := f.service, f.runtime, f.request, f.spool
	ts := httptest.NewServer(f.vault.Server.Handler())
	t.Cleanup(ts.Close)
	ts.Client().Transport = &apiKeyTransport{key: f.key, next: ts.Client().Transport}

	call := func(owner, method, path string, input any) (*http.Response, []byte) {
		t.Helper()
		body, err := json.Marshal(input)
		require.NoError(t, err)
		req, err := http.NewRequestWithContext(ctx, method, ts.URL+path, bytes.NewReader(body))
		require.NoError(t, err)
		req.Header.Set("Content-Type", "application/json")
		if method == http.MethodGet {
			req.Header.Set("X-Api-Key", "")
			req.Header.Set("Range", "bytes=0-3") // A bearer ticket still sends the complete ZIP.
		}
		if owner != "" {
			req.Header.Set(api.WebSessionHeader, owner)
			req.Header.Set("X-Api-Key", "")
		}
		response, err := ts.Client().Do(req)
		require.NoError(t, err)
		body, err = io.ReadAll(response.Body)
		require.NoError(t, err)
		require.NoError(t, response.Body.Close())
		return response, body
	}
	issue := func() string {
		resp, data := call("", "POST", "/api/daemon/web-session", nil)
		require.Equal(t, 201, resp.StatusCode, string(data))
		var session struct {
			Token string `json:"token"`
		}
		require.NoError(t, json.Unmarshal(data, &session))
		return session.Token
	}
	alice, bob := issue(), issue()
	const path = "/api/v1/redaction/recipient-packages"
	resp, body := call(alice, "POST", path, request)
	require.Equal(t, 409, resp.StatusCode, string(body))
	_, err := service.PublishNumberedPDF(ctx, request, runtime)
	require.NoError(t, err)
	gate.entered, gate.once = make(chan struct{}), sync.Once{}
	changed := request
	changed.ReviewSHA256 = strings.Repeat("0", 64)
	resp, body = call(alice, "POST", path, changed)
	require.Equal(t, 409, resp.StatusCode, string(body))
	select {
	case <-gate.entered:
		t.Fatal("changed review reached retained-blob capture")
	default:
	}
	// A legal published Bates label may still be unrepresentable in recipient OPT.
	ns, err := f.vault.EnsureBatesNamespace(ctx, "H,AND", "", 6)
	require.NoError(t, err)
	reserved, err := service.ReserveNumberedPDF(ctx, processing.NumberedReservationRequest{
		Preparation: request.Preparation, Masks: request.Masks, ReviewSHA256: request.ReviewSHA256,
		NamespaceID: uuid.MustParse(ns.NamespaceID), StartAt: 1, RetainOriginal: true,
	}, runtime)
	require.NoError(t, err)
	unimportable := request
	unimportable.AllocationID = reserved.AllocationID
	_, err = service.PublishNumberedPDF(ctx, unimportable, runtime)
	require.NoError(t, err)
	gate.entered, gate.once = make(chan struct{}), sync.Once{}
	resp, body = call(alice, "POST", path, unimportable)
	require.Equal(t, 422, resp.StatusCode, string(body))
	select {
	case <-gate.entered:
		t.Fatal("unimportable labels reached retained-blob capture")
	default:
	}
	resp, body = call(alice, "POST", path, request)
	require.Equal(t, 201, resp.StatusCode, string(body))
	select {
	case <-gate.entered:
	default:
		t.Fatal("successful build did not use verified blob capture")
	}
	var result struct {
		processing.PreparedRecipientPackage

		URL          string `json:"url"`
		Verification string `json:"verification"`
	}
	require.NoError(t, json.Unmarshal(body, &result))
	require.Equal(t, "masks_text_and_numbers", result.Verification)
	resp, body = call(alice, "POST", path, request)
	require.Equal(t, 503, resp.StatusCode, string(body))
	resp, body = call(bob, "DELETE", path, nil)
	require.Equal(t, 204, resp.StatusCode, string(body))
	resp, body = call(alice, "POST", path+"?unknown=1", request)
	require.Equal(t, 403, resp.StatusCode, string(body))
	resp, body = call("", "GET", result.URL, nil)
	require.Equal(t, 200, resp.StatusCode, string(body))
	require.Equal(t, result.Size, int64(len(body)))
	require.NoError(t, processing.VerifyRecipientPackage(ctx, bytes.NewReader(body), int64(len(body)), result.Receipt))
	z, err := zip.NewReader(bytes.NewReader(body), int64(len(body)))
	require.NoError(t, err)
	text, err := z.File[1].Open()
	require.NoError(t, err)
	approved, err := io.ReadAll(text)
	require.NoError(t, err)
	require.NoError(t, text.Close())
	require.Equal(t, "[REDACTED] KEEP SECRET\f", string(approved))
	resp, _ = call("", "GET", result.URL, nil)
	require.Equal(t, 404, resp.StatusCode)
	// Drop the actual successful creation response at a proxy, after publication.
	backend, err := url.Parse(ts.URL)
	require.NoError(t, err)
	proxy := httputil.NewSingleHostReverseProxy(backend)
	published := make(chan int, 1)
	proxy.ModifyResponse = func(response *http.Response) error {
		published <- response.StatusCode
		_ = response.Body.Close()
		return io.ErrUnexpectedEOF
	}
	proxy.ErrorHandler = func(http.ResponseWriter, *http.Request, error) { panic(http.ErrAbortHandler) }
	front := httptest.NewServer(proxy)
	t.Cleanup(front.Close)
	encoded, err := json.Marshal(request)
	require.NoError(t, err)
	lost, err := http.NewRequestWithContext(ctx, http.MethodPost, front.URL+path, bytes.NewReader(encoded))
	require.NoError(t, err)
	lost.Header.Set("Content-Type", "application/json")
	lost.Header.Set(api.WebSessionHeader, alice)
	lost.Header.Set("X-Api-Key", "")
	_, err = ts.Client().Do(lost)
	require.Error(t, err)
	require.Equal(t, 201, <-published, "the reply must be lost after a real package was ready")
	resp, body = call(alice, "DELETE", path, nil)
	require.Equal(t, 204, resp.StatusCode, string(body))
	resp, body = call("", "POST", path, request)
	require.Equal(t, 201, resp.StatusCode, string(body), "API-key clients use the same package build")
	require.NoError(t, json.Unmarshal(body, &result))
	// Reuse the zero-buffer socket harness with the real package and ticket.
	server, client := net.Pipe()
	defer func() { _ = server.Close(); _ = client.Close() }()
	writing := make(chan struct{}, 1)
	output := &previewBlockedResponse{ResponseRecorder: httptest.NewRecorder(), Conn: server, writing: writing}
	finished := make(chan any, 1)
	go func() {
		defer func() { finished <- recover() }()
		ts.Config.Handler.ServeHTTP(output, httptest.NewRequestWithContext(ctx, "GET", ts.URL+result.URL, nil))
	}()
	shutdown, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	select {
	case <-writing:
	case <-shutdown.Done():
		t.Fatal("package never reached the blocked output socket")
	}
	select {
	case result := <-finished:
		t.Fatalf("delivery ended before shutdown: %v", result)
	default:
	}
	require.NoError(t, service.Shutdown(shutdown))
	select {
	case result := <-finished:
		require.Equal(t, http.ErrAbortHandler, result)
	case <-shutdown.Done():
		t.Fatal("shutdown did not interrupt package delivery")
	}
	files, err := filepath.Glob(filepath.Join(spool, "native-pdf-*"))
	require.NoError(t, err)
	require.Empty(t, files)
	_, err = os.Stat(spool)
	require.NoError(t, err)
}

//nolint:paralleltest // Qualified PDFium measures whole-process RSS.
func TestRecipientPackageClearsBodyDeadline(t *testing.T) {
	ready := make(chan struct{})
	close(ready)
	gate := &pausedCitationGate{OperationGate: api.NewOperationGate(), entered: make(chan struct{}), resume: ready}
	f := newRecipientHTTPFixture(t, gate)
	_, err := f.service.PublishNumberedPDF(t.Context(), f.request, f.runtime)
	require.NoError(t, err)
	data, err := json.Marshal(f.request)
	require.NoError(t, err)
	synctest.Test(t, func(t *testing.T) {
		gate.entered, gate.resume = make(chan struct{}), make(chan struct{})
		gate.once = sync.Once{}
		server := httptest.NewTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			r.RemoteAddr = "127.0.0.1:1234"
			f.vault.Server.Handler().ServeHTTP(w, r)
		}))
		server.URL = "http://127.0.0.1"
		server.Client().Transport = &apiKeyTransport{key: f.key, next: server.Client().Transport}
		type result struct {
			status int
			body   []byte
			err    error
		}
		done := make(chan result, 1)
		go func() {
			request, err := http.NewRequestWithContext(t.Context(), http.MethodPost, server.URL+"/api/v1/redaction/recipient-packages", bytes.NewReader(data))
			if err != nil {
				done <- result{err: err}
				return
			}
			request.Header.Set("Content-Type", "application/json")
			request.Host = "localhost"
			resp, err := server.Client().Do(request)
			if err != nil {
				done <- result{err: err}
				return
			}
			body, err := io.ReadAll(resp.Body)
			_ = resp.Body.Close()
			done <- result{status: resp.StatusCode, body: body, err: err}
		}()
		select {
		case <-gate.entered:
		case got := <-done:
			t.Fatalf("build failed before capture: %+v", got)
		}
		// Only virtual time advances while the real retained-byte capture is paused.
		time.Sleep(61 * time.Second)
		synctest.Wait()
		select {
		case got := <-done:
			t.Fatalf("decoded request was canceled by its old body deadline: %+v", got)
		default:
		}
		close(gate.resume)
		got := <-done
		require.NoError(t, got.err)
		require.Equal(t, 201, got.status, string(got.body))
		f.service.RevokeRecipientPackages("")
		// Stop the ticket timer in the virtual-time network that created it.
		require.NoError(t, f.vault.Server.Shutdown(t.Context()))
	})
}
