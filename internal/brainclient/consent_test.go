package brainclient

import (
	"context"
	"encoding/hex"
	"encoding/json/v2"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/daemonauth"
	"go.kenn.io/docbank/internal/daemonconn"
	"go.kenn.io/docbank/internal/processing"
)

func syntheticLocalTextPlan(t *testing.T, state string) api.NoteLocalTextPlan {
	t.Helper()
	name, fixed, err := processing.NewNotePlaintextProfile()
	require.NoError(t, err)
	_, fp, err := document.CanonicalProfile(fixed.Profile)
	require.NoError(t, err)
	inputs := []string{"original_file"}
	retained := []string{"normalized_evidence", "sanitized_markdown"}
	provider := fixed.Profile.Rendition.Descriptor.ID
	return api.NoteLocalTextPlan{Plan: api.ProcessingPlan{Fingerprint: strings.Repeat("a", 64),
		Selector: api.ProcessingSelector{Profile: name}, ProfileFingerprint: fp.Profile,
		Flow: []api.ProcessingFlowHop{{Capability: "rendition", ProviderID: provider, TrustBoundary: string(document.RenditionTrustLocalProcess), InputClasses: inputs,
			RuntimeDisclosure: api.ProcessingRuntimeDisclosure{ImmediateProcessor: provider, UltimateProcessor: provider, Endpoint: "in-process",
				Deployment: fixed.Profile.Rendition.DeploymentFingerprint, MetadataClasses: []string{"byte_length", "content_hash", "detected_media_type", "synthetic_filename"}, RetainedArtifactRoles: retained}}},
		DisclosedClasses: inputs, RetainedClasses: retained, Estimate: api.ProcessingEstimate{ProviderCalls: 1}, ConsentRequired: state != "active", ConsentState: state,
	}, ConsentRequest: document.ProcessingConsentRequest{Principal: "daemon:operator", Scope: "document-processing", ProfileFingerprint: fp.Profile,
		DisclosureFingerprint: fixed.Profile.Rendition.DisclosureFingerprint, InputClasses: inputs, RetainedArtifactClasses: retained}}
}

func TestBrainConnectLocalConsent(t *testing.T) {
	for _, tc := range []struct {
		name, state           string
		index, saved, invalid bool
		wantGrant, wantPlan   bool
	}{
		{name: "required", state: "required", index: true, wantGrant: true, wantPlan: true},
		{name: "active", state: "active", index: true, wantPlan: true},
		{name: "revoked saved connection", state: "revoked", index: true, saved: true, wantGrant: true, wantPlan: true},
		{name: "invalid disclosure", state: "required", index: true, invalid: true, wantPlan: true},
		{name: "omitted", state: "required"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			plan := syntheticLocalTextPlan(t, tc.state)
			if tc.invalid {
				plan.Plan.Flow[0].RuntimeDisclosure.Endpoint = "https://example.invalid"
			}
			var planCalls, grants int
			const key = "synthetic-key"
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				switch r.URL.Path {
				case daemonauth.KeyChallengePath:
					nonce, err := hex.DecodeString(r.URL.Query().Get("nonce"))
					if err != nil {
						http.Error(w, "bad nonce", http.StatusBadRequest)
						return
					}
					_ = json.MarshalWrite(w, map[string]string{"proof": daemonauth.KeyProof(key, nonce)})
				case "/api/v1/documents":
					_ = json.MarshalWrite(w, api.DocumentPage{PublishedNotes: &api.PublishedNoteSelector{Publication: "notes"}, PathPrefix: "/", Sort: "path", Direction: "asc", PageSize: 1, Items: []api.DocumentSummary{}})
				case "/api/v1/notes/local-text-plan":
					planCalls++
					_ = json.MarshalWrite(w, plan)
				case "/api/v1/processing/consents":
					grants++
					var request document.ProcessingConsentRequest
					if err := json.UnmarshalRead(r.Body, &request); err != nil {
						http.Error(w, "bad request", http.StatusBadRequest)
						return
					}
					if request.ProfileFingerprint != plan.ConsentRequest.ProfileFingerprint || request.Scope != plan.ConsentRequest.Scope {
						http.Error(w, "bad consent", http.StatusBadRequest)
						return
					}
					_ = json.MarshalWrite(w, document.ProcessingConsentReceipt{})
				default:
					http.NotFound(w, r)
				}
			}))
			t.Cleanup(server.Close)
			r, err := OpenRegistry(filepath.Join(t.TempDir(), "registry"))
			require.NoError(t, err)
			cfg := ConnectionConfig{Name: "notes", SourceRoot: t.TempDir(), Destination: "/notes", LocalVaultRoot: t.TempDir(), SettleTime: time.Second, ScanInterval: time.Second, IndexLocal: tc.index}
			if tc.saved {
				require.NoError(t, r.Put(t.Context(), cfg))
			}
			client := New(r, Dependencies{Ensure: func(context.Context, string) (*daemonconn.Connection, error) {
				return daemonconn.NewPushConnection(server.URL, key)
			}})
			err = client.Connect(t.Context(), cfg)
			if tc.invalid {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
			}
			require.Equal(t, tc.wantPlan, planCalls > 0)
			require.Equal(t, tc.wantGrant, grants > 0)
			saved, err := r.List(t.Context())
			require.NoError(t, err)
			if tc.invalid {
				require.Empty(t, saved)
			} else {
				require.Len(t, saved, 1)
			}
		})
	}
}

func TestBrainStatusObservesConsentAndTransportFailure(t *testing.T) {
	for _, disconnect := range []bool{false, true} {
		t.Run(fmt.Sprintf("disconnect=%t", disconnect), func(t *testing.T) {
			const key = "synthetic-key"
			plan := syntheticLocalTextPlan(t, "active")
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				switch r.URL.Path {
				case daemonauth.KeyChallengePath:
					nonce, err := hex.DecodeString(r.URL.Query().Get("nonce"))
					if err != nil {
						http.Error(w, "bad nonce", http.StatusBadRequest)
						return
					}
					_ = json.MarshalWrite(w, map[string]string{"proof": daemonauth.KeyProof(key, nonce)})
				case "/api/v1/documents":
					_ = json.MarshalWrite(w, api.DocumentPage{PublishedNotes: &api.PublishedNoteSelector{Publication: "notes"}, PathPrefix: "/", Sort: "path", Direction: "asc", PageSize: 1, Items: []api.DocumentSummary{}})
				case "/api/v1/notes/local-text-plan":
					if disconnect {
						hijacker, ok := w.(http.Hijacker)
						if !ok {
							http.Error(w, "no hijacker", http.StatusInternalServerError)
							return
						}
						conn, _, err := hijacker.Hijack()
						if err == nil {
							_ = conn.Close()
						}
						return
					}
					_ = json.MarshalWrite(w, plan)
				default:
					http.NotFound(w, r)
				}
			}))
			t.Cleanup(server.Close)
			r, err := OpenRegistry(filepath.Join(t.TempDir(), "registry"))
			require.NoError(t, err)
			cfg := ConnectionConfig{Name: "notes", SourceRoot: t.TempDir(), Destination: "/notes", LocalVaultRoot: t.TempDir(), SettleTime: time.Second, ScanInterval: time.Second}
			require.NoError(t, r.Put(t.Context(), cfg))
			client := New(r, Dependencies{Running: func(context.Context, string) (*daemonconn.Connection, bool, error) {
				conn, err := daemonconn.NewPushConnection(server.URL, key)
				return conn, err == nil, err
			}})
			status, err := client.Status(t.Context(), "notes")
			require.NoError(t, err)
			if disconnect {
				require.Equal(t, "unknown", status.ConnectionState)
				require.Equal(t, "daemon_unavailable", status.ProblemCode)
			} else {
				require.Equal(t, "connected", status.ConnectionState)
				require.Equal(t, "authorized", status.BodyIndexingState)
			}
		})
	}
}
