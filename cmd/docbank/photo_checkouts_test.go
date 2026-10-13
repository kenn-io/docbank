package main

import (
	"encoding/hex"
	"encoding/json/v2"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/daemonauth"
	"go.kenn.io/docbank/internal/daemonconn"
	"go.kenn.io/docbank/internal/store"
	"go.kenn.io/docbank/internal/version"
	kitdaemon "go.kenn.io/kit/daemon"
)

func TestPhotoCheckoutCommandSelections(t *testing.T) {
	for _, tc := range []struct {
		args      []string
		years     *store.PhotoCheckoutYears
		all       bool
		wantError bool
	}{
		{args: []string{"estimate", "--years", "2024:2025"}, years: &store.PhotoCheckoutYears{From: 2024, To: 2025}},
		{args: []string{"estimate", "--years", "2024"}, years: &store.PhotoCheckoutYears{From: 2024, To: 2024}},
		{args: []string{"estimate", "--all"}, all: true},
		{args: []string{"estimate", "--years", "bad"}, wantError: true},
	} {
		cmd := newPhotoCheckoutCmd()
		child, remaining, err := cmd.Find(tc.args)
		require.NoError(t, err)
		require.NoError(t, child.ParseFlags(remaining))
		selection, err := checkoutSelection(child)
		if tc.wantError {
			require.Error(t, err)
		} else {
			require.NoError(t, err)
			require.Equal(t, tc.years, selection.Years)
			require.Equal(t, tc.all, selection.All)
		}
	}
}

func TestPhotoCheckoutCreateRejectsInvalidCapacityBeforeDaemon(t *testing.T) {
	t.Setenv("DOCBANK_HOME", t.TempDir())
	for _, tc := range []struct {
		args []string
		want string
	}{
		{[]string{"create", "destination", "--all"}, "positive --byte-limit"},
		{[]string{"create", "destination", "--asset", "00000000-0000-4000-8000-000000000001", "--byte-limit", "-1"}, "--byte-limit must be nonnegative"},
	} {
		cmd := newPhotoCheckoutCmd()
		cmd.SetArgs(tc.args)
		require.ErrorContains(t, cmd.Execute(), tc.want)
	}
}

func TestPhotoCheckoutCLICreateListStatus(t *testing.T) {
	vault := t.TempDir()
	t.Setenv("DOCBANK_HOME", vault)
	const checkoutID = "00000000-0000-4000-8000-000000000001"
	const operationID = "00000000-0000-4000-8000-000000000002"
	const token = "synthetic-proof-token"
	checkout := store.PhotoCheckout{ID: checkoutID, OperationID: operationID, State: "queued", Selection: store.PhotoCheckoutSelection{All: true}, Destination: t.TempDir(), ByteLimit: 100000}
	statusIDs := make(chan string, 1)
	inputs := make(chan api.PhotoCheckoutCreateRequest, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case kitdaemon.DefaultPingPath:
			assert.NoError(t, json.MarshalWrite(w, kitdaemon.PingInfo{OK: true, Service: daemonconn.Service, Version: version.Version, PID: os.Getpid()}))
		case daemonauth.ChallengePath:
			nonce, err := hex.DecodeString(r.URL.Query().Get("nonce"))
			assert.NoError(t, err)
			assert.NoError(t, json.MarshalWrite(w, map[string]string{"proof": daemonauth.Proof(token, nonce)}))
		case "/api/v1/photos/checkouts":
			if r.Method == http.MethodPost {
				var input api.PhotoCheckoutCreateRequest
				assert.NoError(t, json.UnmarshalRead(r.Body, &input))
				inputs <- input
				w.WriteHeader(http.StatusAccepted)
				assert.NoError(t, json.MarshalWrite(w, api.PhotoCheckoutCreateResult{Checkout: checkout, Operation: api.StorageOperation{ID: operationID, Kind: "photo_checkout", State: "queued"}}))
			} else {
				assert.NoError(t, json.MarshalWrite(w, api.PhotoCheckoutList{Items: []store.PhotoCheckout{}}))
			}
		case "/api/v1/photos/checkouts/" + checkoutID:
			statusIDs <- strings.TrimPrefix(r.URL.Path, "/api/v1/photos/checkouts/")
			assert.NoError(t, json.MarshalWrite(w, api.PhotoCheckoutStatus{Checkout: checkout}))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)
	_, err := daemonconn.RuntimeStore(vault).Write(daemonconn.NewRecord(strings.TrimPrefix(server.URL, "http://"), "synthetic-key", token, ""))
	require.NoError(t, err)
	out, err := runCLI(t, "photos", "checkout", "create", checkout.Destination, "--all", "--byte-limit", "100000")
	require.NoError(t, err, out)
	require.Len(t, inputs, 1)
	input := <-inputs
	require.True(t, input.Selection.All)
	require.Equal(t, int64(100000), input.ByteLimit)
	out, err = runCLI(t, "photos", "checkout", "list")
	require.NoError(t, err, out)
	out, err = runCLI(t, "photos", "checkout", "status", checkoutID)
	require.NoError(t, err, out)
	require.Len(t, statusIDs, 1)
	require.Equal(t, checkoutID, <-statusIDs)
}
