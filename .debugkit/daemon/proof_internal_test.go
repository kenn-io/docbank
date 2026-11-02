package daemon

import (
	"bytes"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestProofMACBindsRuntimeIdentity(t *testing.T) {
	key := []byte("identity-proof-key")
	nonce := bytes.Repeat([]byte{0x42}, proofNonceSize)
	rec := RuntimeRecord{
		PID:     123,
		Network: NetworkTCP,
		Address: "127.0.0.1:4321",
		Service: "tool",
		Version: "v1",
	}
	want, err := proofMAC(key, nonce, rec)
	require.NoError(t, err)

	tests := []struct {
		name string
		rec  RuntimeRecord
	}{
		{name: "service", rec: RuntimeRecord{PID: 123, Network: NetworkTCP, Address: "127.0.0.1:4321", Service: "other", Version: "v1"}},
		{name: "version", rec: RuntimeRecord{PID: 123, Network: NetworkTCP, Address: "127.0.0.1:4321", Service: "tool", Version: "v2"}},
		{name: "PID", rec: RuntimeRecord{PID: 456, Network: NetworkTCP, Address: "127.0.0.1:4321", Service: "tool", Version: "v1"}},
		{name: "network", rec: RuntimeRecord{PID: 123, Network: NetworkUnix, Address: "127.0.0.1:4321", Service: "tool", Version: "v1"}},
		{name: "address", rec: RuntimeRecord{PID: 123, Network: NetworkTCP, Address: "127.0.0.1:9876", Service: "tool", Version: "v1"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)

			got, err := proofMAC(key, nonce, tt.rec)
			require.NoError(err)
			assert.NotEqual(want, got)
		})
	}
}

func TestProofPingHandlerRejectsMalformedChallenge(t *testing.T) {
	require := require.New(t)

	proof, err := NewProof([]byte("handler-proof-key"))
	require.NoError(err)
	handler, err := proof.NewPingHandler(RuntimeRecord{
		PID:     123,
		Network: NetworkTCP,
		Address: "127.0.0.1:4321",
		Service: "tool",
	})
	require.NoError(err)

	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, DefaultPingPath, nil)
	req.Header.Set(proofChallengeHeader, "not-a-valid-challenge")
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, req)

	assert.Equal(t, http.StatusBadRequest, recorder.Code)
	assert.Empty(t, recorder.Header().Get(proofResponseHeader))
}

func TestProofProbeRejectsMalformedResponse(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set(proofResponseHeader, "not-a-valid-proof")
		_, _ = fmt.Fprint(w, `{"ok":true,"service":"tool","version":"v1","pid":123}`)
	}))
	defer server.Close()

	rec := RuntimeRecord{
		PID:     123,
		Network: NetworkTCP,
		Address: server.Listener.Addr().String(),
		Service: "tool",
		Version: "v1",
	}
	proof, err := NewProof([]byte("client-proof-key"))
	require.NoError(t, err)
	_, err = proof.probeHTTP(t.Context(), server.Client(), server.URL, rec, ProbeOptions{
		ExpectedService: "tool",
	})
	require.Error(t, err)
}

func TestProofProbeRejectsReplayedResponse(t *testing.T) {
	require := require.New(t)

	key := []byte("replay-proof-key")
	serverProof, err := NewProof(key)
	require.NoError(err)
	server := httptest.NewUnstartedServer(nil)
	rec := RuntimeRecord{
		PID:     123,
		Network: NetworkTCP,
		Address: server.Listener.Addr().String(),
		Service: "tool",
		Version: "v1",
	}
	handler, err := serverProof.NewPingHandler(rec)
	require.NoError(err)
	server.Config.Handler = handler
	server.Start()
	defer server.Close()

	client := *server.Client()
	client.Transport = &replayProofTransport{base: client.Transport}
	clientProof, err := NewProof(key)
	require.NoError(err)
	_, err = clientProof.probeHTTP(t.Context(), &client, server.URL, rec, ProbeOptions{
		ExpectedService: "tool",
	})
	require.NoError(err)
	_, err = clientProof.probeHTTP(t.Context(), &client, server.URL, rec, ProbeOptions{
		ExpectedService: "tool",
	})
	require.Error(err)
}

type replayProofTransport struct {
	base         http.RoundTripper
	firstHeaders http.Header
}

func (t *replayProofTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	resp, err := t.base.RoundTrip(req)
	if err != nil {
		return nil, err
	}
	if t.firstHeaders == nil {
		t.firstHeaders = resp.Header.Clone()
	} else {
		resp.Header = t.firstHeaders.Clone()
	}
	return resp, nil
}
