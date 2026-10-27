package answercard

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/daemonconn"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
)

func TestRewriteLifecyclePreservesContent(t *testing.T) {
	c, err := Parse([]byte(sample))
	if err != nil {
		t.Fatal(err)
	}
	out, err := rewriteLifecycle([]byte(sample), Approved, c.Hash(), "2026-10-27T00:00:00Z", nil)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(out), "\n\n---\n") {
		t.Fatal("rewrite inserted a blank line before closing frontmatter")
	}
	got, err := Parse(out)
	if err != nil {
		t.Fatal(err)
	}
	if got.Effective(nil).State != Approved || got.Hash() != c.Hash() {
		t.Fatalf("changed content or failed approval: %+v", got)
	}
	out, err = rewriteLifecycle(out, Stale, got.ApprovedHash, got.ApprovedAt, &StaleInfo{Reason: "new_version", Citation: 0, ObservedAt: "2026-10-27T00:00:00Z"})
	if err != nil {
		t.Fatal(err)
	}
	got, err = Parse(out)
	if err != nil || got.Effective(nil).State != Stale {
		t.Fatalf("stale: %+v %v", got, err)
	}
}

func TestRewriteRefusesUnknownFields(t *testing.T) {
	input := strings.Replace(sample, "state: draft", "future_field: valuable\nstate: draft", 1)
	if _, err := rewriteLifecycle([]byte(input), Approved, "x", "now", nil); err == nil {
		t.Fatal("discarded unknown field")
	}
}

func TestLatchOnlyNewlyStale(t *testing.T) {
	c, err := Parse([]byte(sample))
	if err != nil {
		t.Fatal(err)
	}
	c.State = Approved
	c.ApprovedHash = c.Hash()
	if !shouldLatch(c, c.Effective([]Observation{{Reason: "new_version", Citation: 0}})) {
		t.Fatal("newly stale card not latched")
	}
	c.State = Stale
	c.Stale = &StaleInfo{Reason: "new_version"}
	if shouldLatch(c, c.Effective(nil)) {
		t.Fatal("already stale card would be rewritten")
	}
	c.State = Approved
	c.Answer += " edited"
	if shouldLatch(c, c.Effective([]Observation{{Reason: "new_version", Citation: 0}})) {
		t.Fatal("edited draft would be latched")
	}
}

func TestApproveRechecksSourcesAfterConfirmation(t *testing.T) {
	card, err := Parse([]byte(sample))
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256([]byte(sample))
	changed := false
	writes := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/api/v1/info":
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{"vault_id": card.Citations[0].VaultUID.String()})
		case strings.HasSuffix(r.URL.Path, "/content") && r.Method == http.MethodGet:
			w.Header().Set(api.ContentVersionHeader, "00000000-0000-4000-8000-000000000003")
			w.Header().Set(api.BlobHashHeader, hex.EncodeToString(digest[:]))
			w.Header().Set(api.BlobSizeHeader, strconv.Itoa(len(sample)))
			w.Header().Set("Trailer", "Content-Digest")
			_, _ = w.Write([]byte(sample))
			w.Header().Set("Content-Digest", "sha-256=:"+base64.StdEncoding.EncodeToString(digest[:])+":")
		case r.URL.Path == "/api/v1/nodes/42":
			n := api.Node{ID: 42, Kind: "file", Path: "/source.txt", CurrentVersionID: card.Citations[0].ContentVersionID.String(), BlobHash: card.Citations[0].ContentSHA256}
			if changed {
				n.TrashedAt = "2026-01-01T00:00:00Z"
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(n)
		case r.URL.Path == "/api/v1/text-citations/resolve":
			h := sha256.Sum256([]byte("test"))
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(document.ResolvedTextCitation{Citation: card.Citations[0].TextCitation(), Text: "test", TextBytes: 4, TextSHA256: hex.EncodeToString(h[:])})
		default:
			writes++
			http.Error(w, "unexpected", http.StatusInternalServerError)
		}
	}))
	defer server.Close()
	n := api.Node{ID: 1, Kind: "file", MimeType: "text/markdown; charset=utf-8", Size: int64(len(sample)), BlobHash: hex.EncodeToString(digest[:]), CurrentVersionID: "00000000-0000-4000-8000-000000000003", Revision: 1}
	err = Approve(t.Context(), daemonconn.New(server.URL, "key"), n, func(Card, []string) (bool, error) { changed = true; return true, nil })
	if err == nil || !strings.Contains(err.Error(), "citation_not_current") {
		t.Fatalf("expected citation_not_current, got %v", err)
	}
	if writes != 0 {
		t.Fatalf("unexpected writes: %d", writes)
	}
}

func TestApprovalRewritePreservesCitationComments(t *testing.T) {
	data := []byte(strings.Replace(sample, "quote: test", `quote: "" # owner review note`, 1))
	card, err := Parse(data)
	if err != nil {
		t.Fatal(err)
	}
	card.Citations[0].Quote = "test"
	out, err := rewriteCard(data, Approved, card.Hash(), "2026-10-27T00:00:00Z", nil, card.Citations)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(out), "# owner review note") {
		t.Fatal("approval lost citation annotation")
	}
	got, err := Parse(out)
	if err != nil {
		t.Fatal(err)
	}
	if got.Effective(nil).State != Approved {
		t.Fatalf("filled approval invalid: %+v", got)
	}
}

func TestApproveRejectsUnsafeYAMLBeforeConfirmation(t *testing.T) {
	data := strings.Replace(sample, "content_sha256: ", "content_sha256: &digest ", 1)
	data = strings.Replace(data, "rendition_attachment_id: "+strings.Repeat("a", 64), "rendition_attachment_id: *digest", 1)
	digest := sha256.Sum256([]byte(data))
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/content") {
			t.Errorf("unexpected request before rejecting unsafe YAML: %s", r.URL.Path)
			http.Error(w, "unexpected", http.StatusInternalServerError)
			return
		}
		w.Header().Set(api.ContentVersionHeader, "00000000-0000-4000-8000-000000000003")
		w.Header().Set(api.BlobHashHeader, hex.EncodeToString(digest[:]))
		w.Header().Set(api.BlobSizeHeader, strconv.Itoa(len(data)))
		w.Header().Set("Trailer", "Content-Digest")
		_, _ = w.Write([]byte(data))
		w.Header().Set("Content-Digest", "sha-256=:"+base64.StdEncoding.EncodeToString(digest[:])+":")
	}))
	defer server.Close()
	n := api.Node{ID: 1, Kind: "file", MimeType: "text/plain", Size: int64(len(data)), BlobHash: hex.EncodeToString(digest[:]), CurrentVersionID: "00000000-0000-4000-8000-000000000003"}
	confirmed := false
	err := Approve(t.Context(), daemonconn.New(server.URL, "key"), n, func(Card, []string) (bool, error) { confirmed = true; return true, nil })
	if err == nil || !strings.Contains(err.Error(), "aliases or merges") || confirmed {
		t.Fatalf("unsafe YAML must be rejected before confirmation: err=%v confirmed=%v", err, confirmed)
	}
}
