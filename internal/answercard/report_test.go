package answercard

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/daemonconn"
)

func TestObserveStalenessAndFailures(t *testing.T) {
	card, err := Parse([]byte(sample))
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name          string
		nodeStatus    int
		node          api.Node
		versionStatus int
		versionNode   int64
		reason        string
		fail          bool
	}{
		{name: "current", node: api.Node{ID: 42, Kind: "file", CurrentVersionID: card.Citations[0].ContentVersionID.String(), BlobHash: card.Citations[0].ContentSHA256}},
		{name: "wrong source digest", node: api.Node{ID: 42, Kind: "file", CurrentVersionID: card.Citations[0].ContentVersionID.String(), BlobHash: "bad"}, reason: "quote_unavailable"},
		{name: "trashed", node: api.Node{ID: 42, Kind: "file", TrashedAt: "2026-01-01T00:00:00Z"}, reason: "trashed"},
		{name: "deleted", nodeStatus: 404, reason: "deleted"},
		{name: "changed", node: api.Node{ID: 42, Kind: "file", CurrentVersionID: "00000000-0000-4000-8000-000000000003"}, reason: "new_version"},
		{name: "pruned", node: api.Node{ID: 42, Kind: "file", CurrentVersionID: "00000000-0000-4000-8000-000000000003"}, versionStatus: 404, reason: "version_pruned"},
		{name: "directory citation", node: api.Node{ID: 42, Kind: "dir"}, reason: "quote_unavailable"},
		{name: "foreign version", node: api.Node{ID: 42, Kind: "file", CurrentVersionID: "00000000-0000-4000-8000-000000000003"}, versionNode: 99, reason: "quote_unavailable"},
		{name: "server failure", nodeStatus: 500, fail: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				if r.URL.Path == "/api/v1/nodes/42" {
					if tc.nodeStatus != 0 {
						w.WriteHeader(tc.nodeStatus)
						code := "internal"
						if tc.nodeStatus == 404 {
							code = "not_found"
						}
						_ = json.NewEncoder(w).Encode(map[string]any{"status": tc.nodeStatus, "code": code, "detail": "synthetic"})
						return
					}
					_ = json.NewEncoder(w).Encode(tc.node)
					return
				}
				if tc.versionStatus != 0 {
					w.WriteHeader(tc.versionStatus)
					_ = json.NewEncoder(w).Encode(map[string]any{"status": tc.versionStatus, "code": "not_found", "detail": "synthetic"})
					return
				}
				versionNode := int64(42)
				if tc.versionNode != 0 {
					versionNode = tc.versionNode
				}
				_ = json.NewEncoder(w).Encode(api.ContentVersion{ID: card.Citations[0].ContentVersionID.String(), NodeID: versionNode})
			}))
			defer server.Close()
			got, err := observe(t.Context(), daemonconn.New(server.URL, "synthetic-key"), card, false)
			if tc.fail {
				if err == nil {
					t.Fatal("expected failure")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if tc.reason == "" {
				if len(got) != 0 {
					t.Fatal(got)
				}
			} else if len(got) != 1 || got[0].Reason != tc.reason {
				t.Fatal(got)
			}
		})
	}
}

func TestDeepObservationDistinguishesUnavailableAndFailure(t *testing.T) {
	card, err := Parse([]byte(sample))
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		code    string
		status  int
		want    string
		fail    bool
		text    string
		deleted bool
	}{{code: "citation_unavailable", status: 404, want: "quote_unavailable"}, {code: "invalid_citation_range", status: 416, want: "quote_unavailable"}, {code: "quote_mismatch", text: "nope", want: "quote_mismatch"}, {code: "citation_unavailable_deleted", status: 404, want: "deleted", deleted: true}, {code: "internal", status: 500, fail: true}} {
		t.Run(tc.code, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				if r.URL.Path == "/api/v1/nodes/42" {
					if tc.deleted {
						w.WriteHeader(http.StatusNotFound)
						_ = json.NewEncoder(w).Encode(map[string]any{"status": 404, "code": "not_found"})
						return
					}
					_ = json.NewEncoder(w).Encode(api.Node{ID: 42, Kind: "file", CurrentVersionID: card.Citations[0].ContentVersionID.String(), BlobHash: card.Citations[0].ContentSHA256})
					return
				}
				if tc.text != "" {
					sum := sha256.Sum256([]byte(tc.text))
					_ = json.NewEncoder(w).Encode(document.ResolvedTextCitation{Citation: card.Citations[0].TextCitation(), Text: tc.text, TextBytes: len(tc.text), TextSHA256: hex.EncodeToString(sum[:])})
					return
				}
				code := tc.code
				if tc.deleted {
					code = "citation_unavailable"
				}
				w.WriteHeader(tc.status)
				_ = json.NewEncoder(w).Encode(map[string]any{"status": tc.status, "code": code, "detail": "synthetic"})
			}))
			defer server.Close()
			got, err := observe(t.Context(), daemonconn.New(server.URL, "synthetic-key"), card, true)
			if tc.fail {
				if err == nil {
					t.Fatal("expected failure")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if tc.deleted {
				if len(got) != 2 || got[0].Reason != "deleted" || got[1].Reason != "quote_unavailable" {
					t.Fatal(got)
				}
				return
			}
			if len(got) != 1 || got[0].Reason != tc.want {
				t.Fatal(got)
			}
		})
	}
}
