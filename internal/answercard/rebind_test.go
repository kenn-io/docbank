package answercard

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/daemonconn"
)

func TestRebindCitationWindowAuthority(t *testing.T) {
	for _, mutation := range []string{"", "reversed", "build", "profile", "checksum", "bytes", "source", "transport"} {
		t.Run(mutation, func(t *testing.T) {
			card, err := Parse([]byte(sample))
			if err != nil {
				t.Fatal(err)
			}
			cite := card.Citations[0]
			text := strings.Repeat("雪", rebindWindowChars-2) + "test tail"
			sum := sha256.Sum256([]byte(text))
			digest := hex.EncodeToString(sum[:])
			selected := api.DocumentRenditionIdentity{ProfileFingerprint: strings.Repeat("b", 64), AttachmentID: strings.Repeat("c", 64), BuildID: strings.Repeat("d", 64)}
			other := api.DocumentRenditionIdentity{ProfileFingerprint: strings.Repeat("e", 64), AttachmentID: strings.Repeat("f", 64), BuildID: strings.Repeat("a", 64)}
			node := api.Node{ID: cite.NodeID, Kind: "file", Path: "/source.txt", CurrentVersionID: "00000000-0000-4000-8000-000000000004", BlobHash: strings.Repeat("b", 64)}
			reads := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				switch r.URL.Path {
				case "/api/v1/nodes/42":
					reads++
					current := node
					if mutation == "source" && reads > 1 {
						current.BlobHash = strings.Repeat("e", 64)
					}
					_ = json.NewEncoder(w).Encode(current)
				case "/api/v1/documents/resolve":
					renditions := []api.DocumentRenditionIdentity{other, selected}
					if mutation == "reversed" {
						renditions[0], renditions[1] = renditions[1], renditions[0]
					}
					_ = json.NewEncoder(w).Encode(api.DocumentSummaryResolveResponse{Items: []api.DocumentSummary{{
						NodeID: node.ID, ContentVersionID: node.CurrentVersionID, Path: node.Path, Name: "source.txt", ModifiedAt: "2026-10-27T00:00:00Z", ActiveRenditions: renditions,
					}}})
				case "/api/v1/renditions/windows":
					var req api.RenditionWindowRequest
					if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
						t.Error(err)
						return
					}
					if req.AttachmentID != selected.AttachmentID {
						t.Error("rendition choice depends on response order")
					}
					if mutation == "transport" && req.Offset > 0 {
						http.Error(w, "unavailable", http.StatusServiceUnavailable)
						return
					}
					part, eof, _ := quoteWindows(text)(req.Offset, req.MaxChars)
					win := api.RenditionTextWindow{VaultID: req.VaultID, NodeID: req.NodeID, ContentVersionID: req.ContentVersionID, AttachmentID: req.AttachmentID,
						BuildID: selected.BuildID, ProfileFingerprint: selected.ProfileFingerprint, Checksum: digest, MediaType: "text/markdown", Text: part, RequestedOffset: req.Offset,
						ActualStart: req.Offset, ActualEnd: req.Offset + len([]rune(part)), NextOffset: req.Offset + len([]rune(part)), EOF: eof, ResponseBytes: len(part)}
					if req.Offset > 0 {
						switch mutation {
						case "build":
							win.BuildID = other.BuildID
						case "profile":
							win.ProfileFingerprint = other.ProfileFingerprint
						case "checksum":
							win.Checksum = strings.Repeat("e", 64)
						case "bytes":
							win.Text = strings.Replace(win.Text, "tail", "fail", 1)
						}
					}
					_ = json.NewEncoder(w).Encode(win)
				default:
					t.Errorf("unexpected route %s", r.URL.Path)
					http.Error(w, "unexpected", http.StatusInternalServerError)
				}
			}))
			defer server.Close()
			got, err := rebindCitation(t.Context(), daemonconn.New(server.URL, "key"), cite)
			if mutation != "" && mutation != "reversed" {
				if err == nil {
					t.Fatalf("accepted %s", mutation)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if got.ContentVersionID.String() != node.CurrentVersionID || got.ContentSHA256 != node.BlobHash || got.Start != rebindWindowChars-2 || got.End != rebindWindowChars+2 || got.BuildID != selected.BuildID || got.RenditionSHA256 != digest {
				t.Fatalf("wrong rebound citation: %+v", got)
			}
		})
	}
}

func TestRebindRewritePreservesCitationComments(t *testing.T) {
	data := []byte(strings.Replace(sample, "start: 0", "start: 0 # owner annotation", 1))
	card, err := Parse(data)
	if err != nil {
		t.Fatal(err)
	}
	card.Citations[0].Start = 9
	card.Citations[0].End = 13
	card.Citations[0].ContentSHA256 = strings.Repeat("b", 64)
	out, err := rewriteCard(data, Approved, card.Hash(), "2026-10-27T00:00:00Z", nil, card.Citations)
	if err != nil {
		t.Fatal(err)
	}
	got, err := Parse(out)
	if err != nil {
		t.Fatal(err)
	}
	if got.Hash() != card.Hash() || got.Effective(nil).State != Approved || !strings.Contains(string(out), "# owner annotation") {
		t.Fatalf("lost rebound content or annotation: %s", out)
	}
}
