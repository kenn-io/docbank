package daemonconn

import (
	"context"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"encoding/json/v2"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestValidateRemoteURL(t *testing.T) {
	for _, raw := range []string{
		"http://archive.example",
		"file:///vault",
		"https://user:pass@archive.example",
		"https://archive.example/#secret",
	} {
		if ValidateRemoteURL(raw) == nil {
			t.Fatalf("accepted unsafe target %q", raw)
		}
	}
	if err := ValidateRemoteURL("https://archive.example"); err != nil {
		t.Fatal(err)
	}
}

const (
	remoteVaultA = "11111111-1111-4111-8111-111111111111"
	remoteVaultB = "22222222-2222-4222-8222-222222222222"
	remoteKey    = "synthetic-remote-key"
)

type syntheticRemoteDaemon struct {
	server       *httptest.Server
	vaultUID     atomic.Value
	capabilities atomic.Int64
	mutations    atomic.Int64
	apiVersion   string
	status       int
}

func newSyntheticRemoteDaemon(t *testing.T, vaultUID string) *syntheticRemoteDaemon {
	t.Helper()
	d := &syntheticRemoteDaemon{apiVersion: RemoteAPIVersion, status: http.StatusOK}
	d.vaultUID.Store(vaultUID)
	d.server = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/tenant/api/v1/capabilities" {
			d.capabilities.Add(1)
			if r.Header.Get("X-Api-Key") != remoteKey || d.status != http.StatusOK {
				status := d.status
				if status == http.StatusOK {
					status = http.StatusUnauthorized
				}
				http.Error(w, "private path /srv/archive and "+remoteKey, status)
				return
			}
			if err := json.MarshalWrite(w, RemoteCapabilities{
				VaultUID: d.currentVaultUID(), APIVersion: d.apiVersion,
				Operations: []string{"admin"}, Limits: map[string]int64{},
			}); err != nil {
				panic(err)
			}
			return
		}
		if strings.HasPrefix(r.URL.Path, "/tenant/api/v1/") && r.Method == http.MethodPost {
			d.mutations.Add(1)
			w.WriteHeader(http.StatusNoContent)
			return
		}
		http.NotFound(w, r)
	}))
	t.Cleanup(d.server.Close)
	return d
}

func (d *syntheticRemoteDaemon) target() RemoteTarget {
	roots := x509.NewCertPool()
	roots.AddCert(d.server.Certificate())
	return RemoteTarget{
		URL: d.server.URL + "/tenant", ExpectedVaultUID: d.currentVaultUID(),
		CredentialRef: "credential:remote", TLSPolicy: RemoteTLSPolicy{RootCAs: roots},
	}
}

func (d *syntheticRemoteDaemon) currentVaultUID() string {
	vaultUID, ok := d.vaultUID.Load().(string)
	if !ok {
		panic("synthetic remote daemon vault UID is not a string")
	}
	return vaultUID
}

func TestConnectTargetNegotiatesPinnedRemoteWithoutLocalFallback(t *testing.T) {
	first := newSyntheticRemoteDaemon(t, remoteVaultA)
	second := newSyntheticRemoteDaemon(t, remoteVaultB)
	var localStarts atomic.Int64
	options := ConnectTargetOptions{
		ResolveCredential: func(_ context.Context, reference string) (string, error) {
			assert.Equal(t, "credential:remote", reference)
			return remoteKey, nil
		},
		EnsureLocal: func(context.Context) (*Connection, error) {
			localStarts.Add(1)
			return nil, errors.New("local daemon must not start")
		},
	}

	connection, capabilities, err := ConnectTarget(t.Context(), new(first.target()), options)
	require.NoError(t, err)
	assert.Equal(t, remoteVaultA, capabilities.VaultUID)
	assert.Equal(t, first.server.URL+"/tenant", connection.base)
	assert.Equal(t, int64(1), first.capabilities.Load())

	_, _, err = ConnectTarget(t.Context(), new(second.target()), options)
	require.NoError(t, err)
	assert.Equal(t, int64(0), localStarts.Load())
}

func TestConnectTargetPreservesLocalSelection(t *testing.T) {
	want := New("http://127.0.0.1:1", "local-key")
	var calls atomic.Int64
	got, capabilities, err := ConnectTarget(t.Context(), nil, ConnectTargetOptions{
		EnsureLocal: func(context.Context) (*Connection, error) {
			calls.Add(1)
			return want, nil
		},
	})
	require.NoError(t, err)
	assert.Same(t, want, got)
	assert.Empty(t, capabilities.VaultUID)
	assert.Equal(t, int64(1), calls.Load())
}

func TestConnectTargetChecksConfiguredCertificatePin(t *testing.T) {
	daemon := newSyntheticRemoteDaemon(t, remoteVaultA)
	target := daemon.target()
	pin := sha256.Sum256(daemon.server.Certificate().RawSubjectPublicKeyInfo)
	target.TLSPolicy.SPKISHA256 = []string{hex.EncodeToString(pin[:])}
	options := ConnectTargetOptions{
		ResolveCredential: func(context.Context, string) (string, error) { return remoteKey, nil },
	}

	connection, _, err := ConnectTarget(t.Context(), &target, options)
	require.NoError(t, err)
	require.NoError(t, connection.Close())

	untrusted := target
	untrusted.TLSPolicy.RootCAs = nil
	_, _, err = ConnectTarget(t.Context(), &untrusted, options)
	require.ErrorIs(t, err, ErrRemoteUnavailable)

	target.TLSPolicy.SPKISHA256 = []string{strings.Repeat("0", sha256.Size*2)}
	_, _, err = ConnectTarget(t.Context(), &target, options)
	require.ErrorIs(t, err, ErrRemoteUnavailable)
}

func TestConnectTargetFailureMatrixNeverStartsLocalDaemon(t *testing.T) {
	first := newSyntheticRemoteDaemon(t, remoteVaultA)
	second := newSyntheticRemoteDaemon(t, remoteVaultB)
	var localStarts atomic.Int64
	baseOptions := ConnectTargetOptions{
		ResolveCredential: func(context.Context, string) (string, error) { return remoteKey, nil },
		EnsureLocal: func(context.Context) (*Connection, error) {
			localStarts.Add(1)
			return nil, errors.New("local daemon must not start")
		},
	}

	t.Run("wrong vault", func(t *testing.T) {
		target := second.target()
		target.ExpectedVaultUID = remoteVaultA
		_, _, err := ConnectTarget(t.Context(), &target, baseOptions)
		require.ErrorIs(t, err, ErrRemoteIdentityMismatch)
	})
	t.Run("missing credential", func(t *testing.T) {
		options := baseOptions
		options.ResolveCredential = func(context.Context, string) (string, error) {
			return "", errors.New("missing /private/credential/file")
		}
		_, _, err := ConnectTarget(t.Context(), new(first.target()), options)
		require.ErrorIs(t, err, ErrRemoteCredentialUnavailable)
		assert.NotContains(t, err.Error(), "/private/credential/file")
	})
	t.Run("unauthorized", func(t *testing.T) {
		options := baseOptions
		options.ResolveCredential = func(context.Context, string) (string, error) { return "wrong", nil }
		_, _, err := ConnectTarget(t.Context(), new(first.target()), options)
		require.ErrorIs(t, err, ErrRemoteUnauthorized)
		assert.NotContains(t, err.Error(), remoteKey)
		assert.NotContains(t, err.Error(), "/srv/archive")
	})
	t.Run("unsupported API", func(t *testing.T) {
		first.apiVersion = "v999"
		t.Cleanup(func() { first.apiVersion = RemoteAPIVersion })
		_, _, err := ConnectTarget(t.Context(), new(first.target()), baseOptions)
		require.ErrorIs(t, err, ErrRemoteAPIIncompatible)
	})
	t.Run("stopped", func(t *testing.T) {
		stopped := newSyntheticRemoteDaemon(t, remoteVaultA)
		target := stopped.target()
		stopped.server.Close()
		_, _, err := ConnectTarget(t.Context(), &target, baseOptions)
		require.ErrorIs(t, err, ErrRemoteUnavailable)
		assert.NotContains(t, err.Error(), stopped.server.URL)
	})

	assert.Equal(t, int64(0), localStarts.Load())
}

func TestRemoteConnectionRechecksVaultBeforeEveryMutation(t *testing.T) {
	daemon := newSyntheticRemoteDaemon(t, remoteVaultA)
	target := daemon.target()
	connection, _, err := ConnectTarget(t.Context(), &target, ConnectTargetOptions{
		ResolveCredential: func(context.Context, string) (string, error) { return remoteKey, nil },
	})
	require.NoError(t, err)

	mutate := func() error {
		req, requestErr := http.NewRequestWithContext(
			t.Context(), http.MethodPost, connection.base+"/api/v1/nodes", nil)
		if requestErr != nil {
			return requestErr
		}
		req.Header.Set("X-Api-Key", remoteKey)
		resp, requestErr := connection.hc.Do(req)
		if requestErr != nil {
			return requestErr
		}
		return resp.Body.Close()
	}

	require.NoError(t, mutate())
	assert.Equal(t, int64(1), daemon.mutations.Load())
	assert.Equal(t, int64(2), daemon.capabilities.Load())

	daemon.vaultUID.Store(remoteVaultB)
	err = mutate()
	require.ErrorIs(t, err, ErrRemoteIdentityMismatch)
	assert.Equal(t, int64(1), daemon.mutations.Load())
	assert.Equal(t, int64(3), daemon.capabilities.Load())
}
