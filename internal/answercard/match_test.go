package answercard

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path"
	"strconv"
	"strings"
	"testing"

	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/daemonconn"
)

func TestMatchLiveRevalidation(t *testing.T) {
	for _, mode := range []string{"fresh", "parameterized text", "oversized", "fractional magnitude", "fractional sign", "fractional comma", "fractional Unicode sign", "unicode root", "source changed", "source changed during quote", "purged", "quote mismatch", "card edited", "card moved", "card trashed", "card deleted", "failure", "not a card", "draft", "invalid"} {
		t.Run(mode, func(t *testing.T) {
			root, prefix := "/answer-cards", "/answer-cards"
			if mode == "unicode root" {
				root = "/Cafe\u0301"
				prefix = "/Café"
			}
			cardPath := prefix + "/card.md"
			input := sample
			question, query := "", ""
			switch mode {
			case "fractional magnitude":
				question, query = "rate .5 percent", "rate 5 percent"
			case "fractional sign":
				question, query = "rate -.5 percent", "rate .5 percent"
			case "fractional comma":
				question, query = "rate ,5 percent", "rate 5 percent"
			case "fractional Unicode sign":
				question, query = "rate −,5 percent", "rate ,5 percent"
			}
			if question != "" {
				input = strings.Replace(input, "When is renewal?", question, 1)
			}
			card, err := Parse([]byte(input))
			if err != nil {
				t.Fatal(err)
			}
			if query == "" {
				query = card.Question
			}
			data := strings.Replace(input, "state: draft", "state: approved\napproved_hash: "+card.Hash(), 1)
			switch mode {
			case "not a card":
				data = "ordinary text"
			case "draft":
				data = sample
			case "invalid":
				data = strings.Replace(data, "question:", "unknown:", 1)
			}
			digest := sha256.Sum256([]byte(data))
			const version = "00000000-0000-4000-8000-000000000003"
			downloads := 0
			resolved := false
			s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				problem := func(status int, code string) {
					w.WriteHeader(status)
					_ = json.NewEncoder(w).Encode(map[string]any{"status": status, "code": code, "detail": "synthetic"})
				}
				switch r.URL.Path {
				case "/api/v1/info":
					_ = json.NewEncoder(w).Encode(map[string]any{"vault_id": card.Citations[0].VaultUID.String()})
				case "/api/v1/documents":
					item := api.DocumentSummary{NodeID: 1, ContentVersionID: version, Path: cardPath, Name: "card.md", MediaType: "text/markdown", Size: int64(len(data)), ModifiedAt: "2026-10-27T00:00:00Z"}
					if mode == "parameterized text" {
						item.MediaType = "text/markdown; charset=utf-8"
					}
					if mode == "oversized" {
						item.Size = MaxBytes + 1
					}
					page := api.DocumentPage{PathPrefix: prefix, Sort: "path", Direction: "asc", PageSize: 250, Items: []api.DocumentSummary{item}}
					_ = json.NewEncoder(w).Encode(page)
				case "/api/v1/versions/" + version + "/content":
					downloads++
					w.Header().Set(api.ContentVersionHeader, version)
					w.Header().Set(api.BlobHashHeader, hex.EncodeToString(digest[:]))
					w.Header().Set(api.BlobSizeHeader, strconv.Itoa(len(data)))
					w.Header().Set("Trailer", "Content-Digest")
					_, _ = w.Write([]byte(data))
					w.Header().Set("Content-Digest", "sha-256=:"+base64.StdEncoding.EncodeToString(digest[:])+":")
				case "/api/v1/nodes/1":
					n := api.Node{ID: 1, Kind: "file", Path: cardPath, CurrentVersionID: version, BlobHash: hex.EncodeToString(digest[:]), Size: int64(len(data))}
					switch mode {
					case "card edited":
						n.CurrentVersionID = "00000000-0000-4000-8000-000000000004"
					case "card moved":
						n.Path = "/elsewhere/card.md"
					case "card trashed":
						n.TrashedAt = "2026-10-27T00:00:00Z"
					case "card deleted":
						problem(http.StatusNotFound, "not_found")
						return
					}
					_ = json.NewEncoder(w).Encode(n)
				case "/api/v1/nodes/42":
					if mode == "failure" {
						problem(http.StatusInternalServerError, "internal")
						return
					}
					n := api.Node{ID: 42, Kind: "file", CurrentVersionID: card.Citations[0].ContentVersionID.String(), BlobHash: card.Citations[0].ContentSHA256}
					if mode == "source changed" || mode == "source changed during quote" && resolved {
						n.CurrentVersionID = "00000000-0000-4000-8000-000000000004"
					}
					_ = json.NewEncoder(w).Encode(n)
				case "/api/v1/versions/" + card.Citations[0].ContentVersionID.String():
					_ = json.NewEncoder(w).Encode(api.ContentVersion{ID: card.Citations[0].ContentVersionID.String(), NodeID: 42})
				case "/api/v1/text-citations/resolve":
					resolved = true
					if mode == "purged" {
						problem(http.StatusNotFound, "citation_unavailable")
						return
					}
					cite := card.Citations[0]
					quote := cite.Quote
					if mode == "quote mismatch" {
						quote = strings.Repeat("x", len(quote))
					}
					sum := sha256.Sum256([]byte(quote))
					_ = json.NewEncoder(w).Encode(document.ResolvedTextCitation{Citation: cite.TextCitation(), Text: quote, TextBytes: len(quote), TextSHA256: hex.EncodeToString(sum[:])})
				default:
					t.Errorf("unexpected route %s", r.URL.Path)
					problem(http.StatusInternalServerError, "internal")
				}
			}))
			defer s.Close()
			m := NewMatcher()
			c := daemonconn.New(s.URL, "synthetic-key")
			got, err := m.Match(t.Context(), c, query, root, 3)
			if mode == "failure" {
				if err == nil {
					t.Fatal("expected fail closed")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if mode == "oversized" {
				if got.Excluded.Invalid != 1 || got.Excluded.NotACard != 0 || downloads != 0 {
					t.Fatalf("oversized candidate classification: %+v downloads=%d", got, downloads)
				}
				return
			}
			if mode != "fresh" && mode != "unicode root" && mode != "parameterized text" {
				if got.Status != "no_match" || len(got.Matches) != 0 {
					t.Fatalf("unsafe match: %+v", got)
				}
				return
			}
			if got.Status != "matched" || len(got.Matches) != 1 || got.Matches[0].Answer != card.Answer {
				t.Fatalf("missing match: %+v", got)
			}
			got, err = m.Match(t.Context(), c, card.Question, root, 3)
			if err != nil || got.Status != "matched" || downloads != 1 {
				t.Fatal(got, err, downloads)
			}
			for _, q := range []string{"unrelated library hours", card.Question + " 2025", "the and"} {
				got, err = m.Match(t.Context(), c, q, root, 3)
				if err != nil || got.Status != "no_match" {
					t.Fatal(q, got, err)
				}
			}
		})
	}
}

func TestMatcherCacheBoundsAndScope(t *testing.T) {
	m := NewMatcher()
	data := make([]byte, MaxBytes)
	for i := range maxCacheBytes/MaxBytes + 1 {
		m.remember(fmt.Sprintf("vault-a/%d", i), data)
	}
	if m.cached("vault-a/0") != nil || m.bytes > maxCacheBytes {
		t.Fatal("cache byte limit not enforced")
	}
	if m.cached("vault-b/1") != nil {
		t.Fatal("cache crossed vault boundary")
	}
	m = NewMatcher()
	for i := range maxCachedVersions + 1 {
		m.remember(strconv.Itoa(i), []byte{1})
	}
	if m.cached("0") != nil || len(m.entries) > maxCachedVersions {
		t.Fatal("cache entry limit not enforced")
	}
}

func TestMatchDiscoveryLimits(t *testing.T) {
	for _, mode := range []string{"files", "bytes", "cursor", "empty"} {
		t.Run(mode, func(t *testing.T) {
			data := strings.Repeat("x", MaxBytes)
			sum := sha256.Sum256([]byte(data))
			const version = "00000000-0000-4000-8000-000000000003"
			requests := 0
			s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				switch r.URL.Path {
				case "/api/v1/info":
					_ = json.NewEncoder(w).Encode(map[string]any{"vault_id": "00000000-0000-4000-8000-000000000001"})
				case "/api/v1/documents":
					requests++
					if mode == "empty" && requests > 2 {
						http.Error(w, "invalid pagination", http.StatusInternalServerError)
						return
					}
					size := int64(0)
					media := "application/pdf"
					if mode == "bytes" {
						size = MaxBytes
						media = "text/plain"
					}
					item := api.DocumentSummary{NodeID: 1, ContentVersionID: version, Path: "/answer-cards/card.md", Name: "card.md", MediaType: media, Size: size, ModifiedAt: "2026-10-27T00:00:00Z"}
					items := make([]api.DocumentSummary, 250)
					for i := range items {
						items[i] = item
						items[i].NodeID = int64(i + 1)
					}
					if mode == "empty" {
						items = nil
					}
					n, _ := strconv.Atoi(r.URL.Query().Get("cursor"))
					cursor := strconv.Itoa(n + 1)
					if mode == "cursor" {
						cursor = "same"
					}
					_ = json.NewEncoder(w).Encode(api.DocumentPage{PathPrefix: "/answer-cards", Sort: "path", Direction: "asc", PageSize: 250, Items: items, NextCursor: cursor})
				case "/api/v1/versions/" + version + "/content":
					w.Header().Set(api.ContentVersionHeader, version)
					w.Header().Set(api.BlobHashHeader, hex.EncodeToString(sum[:]))
					w.Header().Set(api.BlobSizeHeader, strconv.Itoa(len(data)))
					w.Header().Set("Trailer", "Content-Digest")
					_, _ = w.Write([]byte(data))
					w.Header().Set("Content-Digest", "sha-256=:"+base64.StdEncoding.EncodeToString(sum[:])+":")
				default:
					t.Errorf("unexpected route %s", r.URL.Path)
				}
			}))
			defer s.Close()
			got, err := NewMatcher().Match(t.Context(), daemonconn.New(s.URL, "synthetic-key"), "library hours", "/answer-cards", 3)
			if err == nil || len(got.Matches) != 0 {
				t.Fatal("discovery did not fail closed", got, err)
			}
			if mode == "empty" && requests != 1 {
				t.Fatalf("continued through empty pages: %d", requests)
			}
			if mode != "cursor" && mode != "empty" && !errors.Is(err, ErrCardLimit) {
				t.Fatal("missing card_limit", err)
			}
		})
	}
}

func TestMatchRanksPagesAndLimit(t *testing.T) {
	base, err := Parse([]byte(sample))
	if err != nil {
		t.Fatal(err)
	}
	type fixture struct {
		node api.Node
		data []byte
	}
	cards := map[int64]fixture{}
	for id := int64(1); id <= 4; id++ {
		question := "policy renewal"
		alternates := ""
		if id == 1 {
			question = "policy renewal date"
		}
		if id == 2 || id == 4 {
			alternates = "alternates: [policy renewal date]\n"
		}
		raw := strings.Replace(sample, "question: When is renewal?", "question: "+question+"\n"+alternates, 1)
		card, err := Parse([]byte(raw))
		if err != nil {
			t.Fatal(err)
		}
		raw = strings.Replace(raw, "state: draft", "state: approved\napproved_hash: "+card.Hash(), 1)
		sum := sha256.Sum256([]byte(raw))
		name := map[int64]string{1: "d.md", 2: "b.md", 3: "c.md", 4: "a.md"}[id]
		cards[id] = fixture{api.Node{ID: id, Kind: "file", Path: "/answer-cards/" + name, CurrentVersionID: fmt.Sprintf("00000000-0000-4000-8000-%012d", id+10), BlobHash: hex.EncodeToString(sum[:]), Size: int64(len(raw))}, []byte(raw)}
	}
	pages := 0
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/v1/info":
			_ = json.NewEncoder(w).Encode(map[string]any{"vault_id": base.Citations[0].VaultUID.String()})
		case "/api/v1/documents":
			pages++
			ids := []int64{1, 3}
			next := "next"
			if r.URL.Query().Get("cursor") == "next" {
				ids = []int64{2, 4}
				next = ""
			}
			items := []api.DocumentSummary{}
			for _, id := range ids {
				n := cards[id].node
				items = append(items, api.DocumentSummary{NodeID: n.ID, Path: n.Path, Name: path.Base(n.Path), ContentVersionID: n.CurrentVersionID, MediaType: "text/markdown", Size: n.Size, ModifiedAt: "2026-10-27T00:00:00Z"})
			}
			_ = json.NewEncoder(w).Encode(api.DocumentPage{PathPrefix: "/answer-cards", Sort: "path", Direction: "asc", PageSize: 250, Items: items, NextCursor: next})
		case "/api/v1/nodes/42":
			_ = json.NewEncoder(w).Encode(api.Node{ID: 42, Kind: "file", CurrentVersionID: base.Citations[0].ContentVersionID.String(), BlobHash: base.Citations[0].ContentSHA256})
		case "/api/v1/text-citations/resolve":
			quote := base.Citations[0].Quote
			sum := sha256.Sum256([]byte(quote))
			_ = json.NewEncoder(w).Encode(document.ResolvedTextCitation{Citation: base.Citations[0].TextCitation(), Text: quote, TextBytes: len(quote), TextSHA256: hex.EncodeToString(sum[:])})
		default:
			for _, f := range cards {
				if r.URL.Path == fmt.Sprintf("/api/v1/nodes/%d", f.node.ID) {
					_ = json.NewEncoder(w).Encode(f.node)
					return
				}
				if r.URL.Path == "/api/v1/versions/"+f.node.CurrentVersionID+"/content" {
					sum := sha256.Sum256(f.data)
					w.Header().Set(api.ContentVersionHeader, f.node.CurrentVersionID)
					w.Header().Set(api.BlobHashHeader, f.node.BlobHash)
					w.Header().Set(api.BlobSizeHeader, strconv.Itoa(len(f.data)))
					w.Header().Set("Trailer", "Content-Digest")
					_, _ = w.Write(f.data)
					w.Header().Set("Content-Digest", "sha-256=:"+base64.StdEncoding.EncodeToString(sum[:])+":")
					return
				}
			}
			t.Errorf("unexpected route %s", r.URL.Path)
			http.Error(w, "unexpected", http.StatusInternalServerError)
		}
	}))
	defer s.Close()
	got, err := NewMatcher().Match(t.Context(), daemonconn.New(s.URL, "synthetic-key"), "policy renewal", "/answer-cards", 3)
	if err != nil {
		t.Fatal(err)
	}
	if pages != 2 || len(got.Matches) != 3 {
		t.Fatal(pages, got)
	}
	for i, id := range []int64{4, 2, 3} {
		if got.Matches[i].Card.NodeID != id {
			t.Fatalf("wrong rank %d: %+v", i, got.Matches)
		}
	}
}
