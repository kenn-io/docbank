package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"mime/multipart"
	"net"
	"net/http"
	"net/http/httptest"
	"net/textproto"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/internal/conversionworker"
)

const (
	testWorkerAPIKey    = "synthetic-worker-api-key"
	testBackendAPIKey   = "synthetic-backend-api-key"
	testWorkerRequestID = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
)

func workerCommandEnvironment(name string) string {
	switch name {
	case "DOCBANK_WORKER_API_KEY":
		return testWorkerAPIKey
	case "DOCBANK_WORKER_BACKEND_KEY":
		return testBackendAPIKey
	default:
		return ""
	}
}

func TestWorkerCommandRejectsUnsafeConfiguration(t *testing.T) {
	err := run(t.Context(), []string{"--backend", "http://127.0.0.1:5001"}, workerCommandEnvironment, io.Discard)
	require.EqualError(t, err, "worker directory is required")

	err = run(t.Context(), []string{"--directory", t.TempDir(), "--backend", "http://203.0.113.1:5001"}, workerCommandEnvironment, io.Discard)
	require.ErrorContains(t, err, "configure conversion worker:")
	require.ErrorContains(t, err, "worker backend must be loopback")
}

func TestWorkerCommandRejectsZeroLimits(t *testing.T) {
	for _, field := range []string{"max-source-bytes", "max-result-bytes", "max-jobs", "max-spool-bytes"} {
		t.Run(field, func(t *testing.T) {
			spool := filepath.Join(t.TempDir(), "spool")
			err := run(t.Context(), []string{"--directory", spool, "--backend", "http://127.0.0.1:5001", "--" + field + "=0"}, workerCommandEnvironment, io.Discard)
			require.ErrorContains(t, err, field+" must be between")
		})
	}
	for _, key := range []string{"DOCBANK_WORKER_API_KEY", "DOCBANK_WORKER_BACKEND_KEY"} {
		t.Run(key, func(t *testing.T) {
			spool := filepath.Join(t.TempDir(), "spool")
			environment := func(name string) string {
				if name == key {
					return "synthetic-invalid\nkey"
				}
				return workerCommandEnvironment(name)
			}
			err := run(t.Context(), []string{"--directory", spool, "--backend", "http://127.0.0.1:5001"}, environment, io.Discard)
			require.ErrorContains(t, err, key)
		})
	}
}

func TestWorkerCommandCancellationReleasesSpool(t *testing.T) {
	backendReady := make(chan struct{})
	backend := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/ready" {
			http.Error(response, "unexpected backend request", http.StatusBadRequest)
			return
		}
		close(backendReady)
		<-request.Context().Done()
	}))
	defer backend.Close()

	options := conversionworker.Options{
		Directory:      filepath.Join(t.TempDir(), "spool"),
		APIKey:         testWorkerAPIKey,
		BackendURL:     backend.URL,
		BackendKey:     testBackendAPIKey,
		MaxSourceBytes: 64 << 20,
		MaxResultBytes: 64 << 20,
		MaxJobs:        128,
		MaxSpoolBytes:  40 << 30,
	}
	seedWorkerCommandJob(t, options)
	listenConfig := net.ListenConfig{}
	reservedListener, err := listenConfig.Listen(t.Context(), "tcp", "127.0.0.1:0")
	require.NoError(t, err)
	listenAddress := reservedListener.Addr().String()
	require.NoError(t, reservedListener.Close())

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		done <- run(ctx, []string{"--directory", options.Directory, "--listen", listenAddress, "--backend", options.BackendURL}, workerCommandEnvironment, io.Discard)
	}()
	select {
	case <-backendReady:
		cancel()
	case err := <-done:
		require.FailNowf(t, "command returned before its runner acquired the spool", "%v", err)
	}
	if err := <-done; err != nil {
		require.ErrorIs(t, err, context.Canceled)
	}

	reopened, err := conversionworker.Open(t.Context(), options)
	require.NoError(t, err)
	request := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/v1/jobs/by-key/"+testWorkerRequestID, nil)
	request.Header.Set("X-Api-Key", options.APIKey)
	response := httptest.NewRecorder()
	reopened.ServeHTTP(response, request)
	require.Equal(t, http.StatusOK, response.Code, response.Body.String())
	require.NoError(t, reopened.Close(context.WithoutCancel(t.Context())))
}

func seedWorkerCommandJob(t *testing.T, options conversionworker.Options) {
	t.Helper()
	worker, err := conversionworker.Open(t.Context(), options)
	require.NoError(t, err)
	defer func() { require.NoError(t, worker.Close(context.WithoutCancel(t.Context()))) }()

	source := []byte("synthetic source")
	var body bytes.Buffer
	multipartBody := multipart.NewWriter(&body)
	header := make(textproto.MIMEHeader)
	header.Set("Content-Disposition", multipart.FileContentDisposition("files", "synthetic.pdf"))
	header.Set("Content-Type", "application/pdf")
	part, err := multipartBody.CreatePart(header)
	require.NoError(t, err)
	_, err = part.Write(source)
	require.NoError(t, err)
	require.NoError(t, multipartBody.WriteField("to_formats", "json"))
	require.NoError(t, multipartBody.WriteField("target_type", "inbody"))
	require.NoError(t, multipartBody.Close())

	request := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/v1/convert/file/async", &body)
	request.Header.Set("Content-Type", multipartBody.FormDataContentType())
	request.Header.Set("X-Api-Key", options.APIKey)
	request.Header.Set("Idempotency-Key", testWorkerRequestID)
	digest := sha256.Sum256(source)
	request.Header.Set("X-Docbank-Source-Sha256", hex.EncodeToString(digest[:]))
	response := httptest.NewRecorder()
	worker.ServeHTTP(response, request)
	require.Equal(t, http.StatusAccepted, response.Code, response.Body.String())
}

func TestWorkerCommandRejectsListenerConflicts(t *testing.T) {
	for _, test := range []struct{ listen, backend, want string }{
		{"127.0.0.1:5002", "http://127.0.0.1:5002", "backend must differ from worker listener"},
		{"[::1]:5002", "http://[::1]:5002", "backend must differ from worker listener"},
		{"127.0.0.1:80", "http://127.0.0.1", "backend must differ from worker listener"},
		{"[::ffff:127.0.0.1]:5002", "http://127.0.0.1:5002", "backend must differ from worker listener"},
		{"0.0.0.0:5002", "http://127.0.0.1:5002", "listener must be an explicit"},
		{"[::]:5002", "http://127.0.0.1:5002", "listener must be an explicit"},
	} {
		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		err := run(ctx, []string{"--directory", t.TempDir(), "--listen", test.listen, "--backend", test.backend}, func(string) string { return "synthetic-valid-worker-key" }, io.Discard)
		require.ErrorContains(t, err, test.want)
	}
}
