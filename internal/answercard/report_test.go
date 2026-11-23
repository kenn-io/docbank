package answercard

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
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
func TestCheckRunSkipsDisappearedCard(t *testing.T) {
	card, err := Parse([]byte(sample))
	if err != nil {
		t.Fatal(err)
	}
	data := strings.Replace(sample, "state: draft", "state: approved\napproved_hash: "+card.Hash(), 1)
	digest := sha256.Sum256([]byte(data))
	const cardVersion = "00000000-0000-4000-8000-000000000003"
	for _, status := range []int{http.StatusNotFound, http.StatusForbidden, http.StatusInternalServerError} {
		t.Run(strconv.Itoa(status), func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				switch r.URL.Path {
				case "/api/v1/info":
					_ = json.NewEncoder(w).Encode(map[string]any{"vault_id": card.Citations[0].VaultUID.String()})
				case "/api/v1/documents":
					_ = json.NewEncoder(w).Encode(api.DocumentPage{
						PathPrefix: "/answer-cards", Sort: "path", Direction: "asc", PageSize: 250,
						Items: []api.DocumentSummary{{NodeID: 1, ContentVersionID: cardVersion,
							Path: "/answer-cards/card.md", Name: "card.md", MediaType: "text/markdown; charset=utf-8",
							Size: int64(len(data)), ModifiedAt: "2026-10-27T00:00:00Z"}},
					})
				case "/api/v1/versions/" + cardVersion + "/content":
					w.Header().Set(api.ContentVersionHeader, cardVersion)
					w.Header().Set(api.BlobHashHeader, hex.EncodeToString(digest[:]))
					w.Header().Set(api.BlobSizeHeader, strconv.Itoa(len(data)))
					w.Header().Set("Trailer", "Content-Digest")
					_, _ = w.Write([]byte(data))
					w.Header().Set("Content-Digest", "sha-256=:"+base64.StdEncoding.EncodeToString(digest[:])+":")
				case "/api/v1/nodes/42":
					_ = json.NewEncoder(w).Encode(api.Node{ID: 42, Kind: "file", TrashedAt: "2026-10-27T00:00:00Z"})
				case "/api/v1/nodes/1":
					code := "internal"
					if status == http.StatusNotFound {
						code = "not_found"
					}
					if status == http.StatusForbidden {
						code = "forbidden"
					}
					w.WriteHeader(status)
					_ = json.NewEncoder(w).Encode(map[string]any{"status": status, "code": code, "detail": "synthetic"})
				default:
					t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
					http.Error(w, "unexpected", http.StatusInternalServerError)
				}
			}))
			defer server.Close()
			report, err := CheckRun(t.Context(), daemonconn.New(server.URL, "synthetic-key"), "/answer-cards", false)
			if status != http.StatusNotFound {
				if err == nil {
					t.Fatal("operational failure did not abort report")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if len(report.Items) != 1 || report.Items[0].Reason != "stale_revision" {
				t.Fatalf("disappeared card not reported as skipped: %+v", report)
			}
		})
	}
}

func TestCheckRunSkipsCardDisappearedDuringReplacement(t *testing.T) {
	card, err := Parse([]byte(sample))
	if err != nil {
		t.Fatal(err)
	}
	data := strings.Replace(sample, "state: draft", "state: approved\napproved_hash: "+card.Hash(), 1)
	digest := sha256.Sum256([]byte(data))
	const cardVersion = "00000000-0000-4000-8000-000000000003"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/v1/info":
			_ = json.NewEncoder(w).Encode(map[string]any{"vault_id": card.Citations[0].VaultUID.String()})
		case "/api/v1/documents":
			_ = json.NewEncoder(w).Encode(api.DocumentPage{
				PathPrefix: "/answer-cards", Sort: "path", Direction: "asc", PageSize: 250,
				Items: []api.DocumentSummary{
					{NodeID: 1, ContentVersionID: cardVersion, Path: "/answer-cards/card.md", Name: "card.md", MediaType: "text/markdown; charset=utf-8", Size: int64(len(data)), ModifiedAt: "2026-10-27T00:00:00Z"},
					{NodeID: 2, ContentVersionID: "00000000-0000-4000-8000-000000000004", Path: "/answer-cards/later.bin", Name: "later.bin", MediaType: "application/octet-stream", ModifiedAt: "2026-10-27T00:00:00Z"},
				},
			})
		case "/api/v1/versions/" + cardVersion + "/content":
			w.Header().Set(api.ContentVersionHeader, cardVersion)
			w.Header().Set(api.BlobHashHeader, hex.EncodeToString(digest[:]))
			w.Header().Set(api.BlobSizeHeader, strconv.Itoa(len(data)))
			w.Header().Set("Trailer", "Content-Digest")
			_, _ = w.Write([]byte(data))
			w.Header().Set("Content-Digest", "sha-256=:"+base64.StdEncoding.EncodeToString(digest[:])+":")
		case "/api/v1/nodes/42":
			_ = json.NewEncoder(w).Encode(api.Node{ID: 42, Kind: "file", TrashedAt: "2026-10-27T00:00:00Z"})
		case "/api/v1/nodes/1":
			_ = json.NewEncoder(w).Encode(api.Node{ID: 1, Kind: "file", Path: "/answer-cards/card.md", CurrentVersionID: cardVersion, Revision: 4, MimeType: "text/markdown"})
		case "/api/v1/nodes/1/content":
			if r.Method != http.MethodPut {
				t.Errorf("replacement used %s, want PUT", r.Method)
			}
			w.WriteHeader(http.StatusNotFound)
			_ = json.NewEncoder(w).Encode(map[string]any{"status": http.StatusNotFound, "code": "not_found", "detail": "synthetic"})
		default:
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
			http.Error(w, "unexpected", http.StatusInternalServerError)
		}
	}))
	defer server.Close()

	report, err := CheckRun(t.Context(), daemonconn.New(server.URL, "synthetic-key"), "/answer-cards", false)
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Items) != 2 {
		t.Fatalf("report did not continue after the disappeared card: %+v", report.Items)
	}
	if got := report.Items[0]; got.State != "stale" || got.Reason != "stale_revision" {
		t.Fatalf("disappeared card row = %+v, want stale_revision", got)
	}
	if got := report.Items[1]; got.Path != "/answer-cards/later.bin" || got.State != "not_a_card" {
		t.Fatalf("later row = %+v, want the report to continue", got)
	}
}

func TestCheckRunSkipsUnsafeYAML(t *testing.T) {
	card, err := Parse([]byte(sample))
	if err != nil {
		t.Fatal(err)
	}
	data := strings.Replace(sample, "content_sha256: ", "content_sha256: &digest ", 1)
	data = strings.Replace(data, "rendition_attachment_id: "+strings.Repeat("a", 64), "rendition_attachment_id: *digest", 1)
	data = strings.Replace(data, "state: draft", "state: approved\napproved_hash: "+card.Hash(), 1)
	digest := sha256.Sum256([]byte(data))
	const version = "00000000-0000-4000-8000-000000000003"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/v1/info":
			_ = json.NewEncoder(w).Encode(map[string]any{"vault_id": card.Citations[0].VaultUID.String()})
		case "/api/v1/documents":
			_ = json.NewEncoder(w).Encode(api.DocumentPage{PathPrefix: "/answer-cards", Sort: "path", Direction: "asc", PageSize: 250, Items: []api.DocumentSummary{{NodeID: 1, ContentVersionID: version, Path: "/answer-cards/card.md", Name: "card.md", MediaType: "text/markdown", Size: int64(len(data)), ModifiedAt: "2026-10-27T00:00:00Z"}}})
		case "/api/v1/versions/" + version + "/content":
			w.Header().Set(api.ContentVersionHeader, version)
			w.Header().Set(api.BlobHashHeader, hex.EncodeToString(digest[:]))
			w.Header().Set(api.BlobSizeHeader, strconv.Itoa(len(data)))
			w.Header().Set("Trailer", "Content-Digest")
			_, _ = w.Write([]byte(data))
			w.Header().Set("Content-Digest", "sha-256=:"+base64.StdEncoding.EncodeToString(digest[:])+":")
		case "/api/v1/nodes/42":
			_ = json.NewEncoder(w).Encode(api.Node{ID: 42, Kind: "file", TrashedAt: "2026-10-27T00:00:00Z"})
		case "/api/v1/nodes/1":
			_ = json.NewEncoder(w).Encode(api.Node{ID: 1, Kind: "file", Path: "/answer-cards/card.md", CurrentVersionID: version})
		default:
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
			http.Error(w, "unexpected", http.StatusInternalServerError)
		}
	}))
	defer server.Close()
	report, err := CheckRun(t.Context(), daemonconn.New(server.URL, "key"), "/answer-cards", false)
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Items) != 1 || report.Items[0].Reason != "rewrite_unsupported" {
		t.Fatalf("unsafe card not reported as skipped: %+v", report)
	}
}
