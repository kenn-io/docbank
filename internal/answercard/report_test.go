package answercard

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/daemonconn"
)

func TestCheckReportsStalledDocumentListing(t *testing.T) {
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/v1/info":
			_ = json.NewEncoder(w).Encode(map[string]any{"vault_id": "00000000-0000-4000-8000-000000000001"})
		case "/api/v1/documents":
			requests++
			_ = json.NewEncoder(w).Encode(api.DocumentPage{
				PathPrefix: "/answer-cards", Sort: "path", Direction: "asc", PageSize: 250,
				NextCursor: "same",
			})
		default:
			t.Errorf("unexpected route %s", r.URL.Path)
			http.Error(w, "unexpected route", http.StatusInternalServerError)
		}
	}))
	defer server.Close()

	_, err := Check(t.Context(), daemonconn.New(server.URL, "synthetic-key"), "/answer-cards", false)
	if err == nil || err.Error() != "Document listing stalled." {
		t.Fatalf("want stalled-listing message, got %v", err)
	}
	if requests != 2 {
		t.Fatalf("want two requests before repeated cursor detection, got %d", requests)
	}
}

// Exercise limits through the catalog and content endpoints with synthetic files.
func TestCheckCandidateBounds(t *testing.T) {
	for _, tc := range []struct {
		name       string
		files      int
		size       int64
		media      string
		streamSize int64
		badHash    bool
		wantError  string
		wantState  string
	}{
		{name: "file limit", files: MaxFiles + 1, media: "application/pdf", wantError: "card_limit: too many files"},
		{name: "download limit", files: MaxDownloadBytes/MaxBytes + 1, size: MaxBytes, media: "text/plain", wantError: "card_limit: download limit exceeded"},
		{name: "oversized candidate", files: 1, size: MaxBytes + 1, media: "text/plain", wantState: "invalid"},
		{name: "content size changed", files: 1, size: 1, streamSize: 2, media: "text/plain", wantError: "card content identity changed"},
		{name: "content digest mismatch", files: 1, size: 1, badHash: true, media: "text/plain", wantError: "computed SHA-256"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pageNumber, downloads := 0, 0
			body := strings.Repeat("x", int(tc.size))
			sum := sha256.Sum256([]byte(body))
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/api/v1/info":
					w.Header().Set("Content-Type", "application/json")
					_ = json.NewEncoder(w).Encode(map[string]any{"vault_id": "00000000-0000-4000-8000-000000000001"})
				case "/api/v1/documents":
					w.Header().Set("Content-Type", "application/json")
					page := api.DocumentPage{PathPrefix: "/answer-cards", Sort: "path", Direction: "asc", PageSize: 250}
					start := pageNumber * 250
					for i := start; i < min(start+250, tc.files); i++ {
						name := fmt.Sprintf("%04d.txt", i)
						version := fmt.Sprintf("00000000-0000-4000-8000-%012x", i+1)
						page.Items = append(page.Items, api.DocumentSummary{NodeID: int64(i + 1), ContentVersionID: version, Path: "/answer-cards/" + name, Name: name, MediaType: tc.media, Size: tc.size, ModifiedAt: "2026-01-01T00:00:00Z"})
					}
					pageNumber++
					if start+250 < tc.files {
						page.NextCursor = strconv.Itoa(pageNumber)
					}
					_ = json.NewEncoder(w).Encode(page)
				default:
					downloads++
					size := tc.size
					if tc.streamSize != 0 {
						size = tc.streamSize
					}
					w.Header().Set(api.ContentVersionHeader, strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/api/v1/versions/"), "/content"))
					hash := hex.EncodeToString(sum[:])
					if tc.badHash {
						hash = strings.Repeat("0", 64)
					}
					w.Header().Set(api.BlobHashHeader, hash)
					w.Header().Set(api.BlobSizeHeader, strconv.FormatInt(size, 10))
					w.Header().Set("Trailer", "Content-Digest")
					_, _ = w.Write([]byte(body))
					w.Header().Set("Content-Digest", "sha-256=:"+base64.StdEncoding.EncodeToString(sum[:])+":")
				}
			}))
			t.Cleanup(server.Close)
			got, err := Check(t.Context(), daemonconn.New(server.URL, "synthetic-key"), "/answer-cards", false)
			if tc.wantError != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantError) {
					t.Fatalf("want %q, got %v", tc.wantError, err)
				}
			} else if err != nil || len(got.Items) != 1 || got.Items[0].State != tc.wantState || got.Items[0].Reason != "oversized" {
				t.Fatalf("unexpected report %+v, error %v", got, err)
			}
			if tc.name == "oversized candidate" && downloads != 0 {
				t.Fatal("oversized candidate was downloaded")
			}
			if tc.name == "download limit" && downloads != MaxDownloadBytes/MaxBytes {
				t.Fatalf("downloaded %d candidates", downloads)
			}
		})
	}
}

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

func TestDeepObservationDeduplicatesQuoteUnavailable(t *testing.T) {
	card, err := Parse([]byte(sample))
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name     string
		kind     string
		blobHash string
	}{
		{name: "changed source digest", kind: "file", blobHash: "changed"},
		{name: "directory citation", kind: "dir", blobHash: card.Citations[0].ContentSHA256},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				if r.URL.Path == "/api/v1/nodes/42" {
					_ = json.NewEncoder(w).Encode(api.Node{
						ID: 42, Kind: tc.kind,
						CurrentVersionID: card.Citations[0].ContentVersionID.String(),
						BlobHash:         tc.blobHash,
					})
					return
				}
				w.WriteHeader(http.StatusNotFound)
				_ = json.NewEncoder(w).Encode(map[string]any{
					"status": http.StatusNotFound,
					"code":   "citation_unavailable",
					"detail": "synthetic",
				})
			}))
			defer server.Close()

			got, err := observe(t.Context(), daemonconn.New(server.URL, "synthetic-key"), card, true)
			if err != nil {
				t.Fatal(err)
			}
			if len(got) != 1 || got[0].Citation != 0 || got[0].Reason != "quote_unavailable" {
				t.Fatalf("want one quote_unavailable issue for citation 0, got %+v", got)
			}
		})
	}
}
