package mcp

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/document/plaintext"
	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/config"
	"go.kenn.io/docbank/internal/daemonconn"
	"go.kenn.io/docbank/internal/processing"
	"go.kenn.io/docbank/internal/store"
)

func TestFindConflictsReportsDifferingFigures(t *testing.T) {
	f := newNativeExportFixture(t)
	var identities []api.DocumentIdentity
	versions := []string{}
	for _, file := range []struct{ name, sentence string }{
		{"cooling period 2025.txt", strings.Repeat("General policy information applies to all accounts. ", 6) + "The cooling period is 7 days. Contact support for help. General terms apply to all accounts."},
		{"policy-2026.txt", "The cooling period is 15 days."},
		{"cooling period notes.txt", ""},
		{"premium-policy.txt", "The premium cooling period is 30 days."},
	} {
		text := file.sentence
		member := f.addFile(t, file.name, text)
		if file.sentence != "" {
			require.NoError(t, f.catalog.RecordExtraction(t.Context(), store.ExtractionResult{
				BlobHash: member.SHA256, Extractor: "plain-text", ExtractorVersion: 1,
				Status: store.ExtractionOK, Text: text,
			}))
		}
		identities = append(identities, api.DocumentIdentity{NodeID: member.NodeID, ContentVersionID: member.VersionID, Path: "/" + file.name})
		versions = append(versions, member.VersionID)
	}
	daemon, service, profile := newProcessingDocumentServer(t, f)
	reader := api.NewDocumentReader(f.catalog, f.blobs, os.Getenv("DOCBANK_HOME"), service)
	docs, err := reader.ResolveDocumentSummaries(t.Context(), api.DocumentSummaryResolveRequest{Identities: identities})
	require.NoError(t, err)
	search, err := daemonRead(t.Context(), daemon.daemon, func(ctx context.Context, c *daemonconn.Connection) (documentSearchResponse, error) {
		return runDocumentSearch(ctx, daemonDocumentClient{c}, searchDocumentsInput{Query: "cooling period", Profile: profile, Mode: "lexical", Filters: &api.DocumentSourceFenceFilters{}}, true, nil)
	})
	require.NoError(t, err)
	var excerpt string
	for _, hit := range search.report.Results {
		if hit.Path == "/cooling period 2025.txt" {
			excerpt = hit.Excerpt
		}
	}
	require.True(t, strings.HasPrefix(excerpt, " … The cooling period is 7 days."), excerpt)
	for _, scope := range []string{"filters", "content_version_ids"} {
		t.Run(scope, func(t *testing.T) {
			args := map[string]any{"query": "cooling period", "profile": profile}
			if scope == "filters" {
				args[scope] = map[string]any{}
			} else {
				args[scope] = versions
			}
			out := exportCall(t, daemon, "find_conflicts", args)
			require.Equal(t, "CONFLICTING", out["status"])
			passages := conflictField[[]any](t, out, "passages")
			require.Len(t, passages, 4)
			for _, entry := range passages {
				p := schemaMap(t, entry)
				path := conflictField[string](t, p, "path")
				require.Equal(t, path == "/cooling period 2025.txt" || path == "/policy-2026.txt", p["compared"])
				require.NotEmpty(t, p["evidence_ids"])
				for _, doc := range docs {
					if doc.Path == path {
						require.Equal(t, doc.ModifiedAt, p["modified_at"])
						require.EqualValues(t, doc.NodeID, p["node_id"])
						require.Equal(t, doc.ContentVersionID, p["content_version_id"])
					}
				}
			}
			conflicts := conflictField[[]any](t, out, "conflicts")
			require.Len(t, conflicts, 1)
			conflict := schemaMap(t, conflicts[0])
			require.Equal(t, "duration", conflict["kind"])
			require.Equal(t, "day", conflict["unit"])
			figures := conflictField[[]any](t, conflict, "figures")
			require.Len(t, figures, 2)
			quotes := map[string]string{}
			for _, entry := range figures {
				figure := schemaMap(t, entry)
				quotes[conflictField[string](t, figure, "value")] = conflictField[string](t, figure, "quote")
				index := int(conflictField[float64](t, figure, "passage")) - 1
				p := schemaMap(t, passages[index])
				require.Equal(t, map[string]string{"7": "/cooling period 2025.txt", "15": "/policy-2026.txt"}[conflictField[string](t, figure, "value")], p["path"])
			}
			require.Equal(t, map[string]string{"7": "The cooling period is 7 days.", "15": "The cooling period is 15 days."}, quotes)
		})
	}
}

func TestFindConflictsRejectsRetrievalOptions(t *testing.T) {
	for _, option := range []struct {
		key   string
		value any
	}{{"mode", "semantic"}, {"binding_id", "synthetic-binding"}, {"explain", true}, {"rerank", true}, {"limit", 21}, {"as_of", "2024-06-01"}} {
		t.Run(option.key+fmt.Sprint(option.value), func(t *testing.T) {
			args := map[string]any{"query": "cooling period", "profile": "local", "filters": map[string]any{}, option.key: option.value}
			wire := callToolWire(t, "find_conflicts", args)
			wireErr := decodeWireError(t, wire)
			require.EqualValues(t, jsonrpc.CodeInvalidParams, wireErr.Code)
			require.Equal(t, "invalid tool arguments", wireErr.Message)
		})
	}
}

func TestFindConflictsReadsEmailRendition(t *testing.T) {
	f := newNativeExportFixture(t)
	body := "The cooling period is 30 days. Contact support for help."
	hash, size, err := f.blobs.WriteContext(t.Context(), strings.NewReader("MIME-Version: 1.0\r\nContent-Type: text/plain; charset=utf-8\r\n\r\n"+body+"\r\n"))
	require.NoError(t, err)
	node, err := f.catalog.CreateFile(t.Context(), f.catalog.RootID(), "policy.eml", hash, size, "message/rfc822")
	require.NoError(t, err)
	version, err := f.catalog.ContentVersionByID(t.Context(), node.CurrentVersionID)
	require.NoError(t, err)
	spool, err := filepath.EvalSymlinks(t.TempDir())
	require.NoError(t, err)
	_, err = processing.EnsureEmailTarget(t.Context(), f.catalog, f.blobs, spool, store.EmailTarget{Version: version})
	require.NoError(t, err)
	member := f.addFile(t, "policy.txt", "The cooling period is 15 days.")
	require.NoError(t, f.catalog.RecordExtraction(t.Context(), store.ExtractionResult{BlobHash: member.SHA256, Extractor: "plain-text", ExtractorVersion: 1, Status: store.ExtractionOK, Text: "The cooling period is 15 days."}))
	daemon, _, profile := newProcessingDocumentServer(t, f)
	out := exportCall(t, daemon, "find_conflicts", map[string]any{"query": "cooling period", "profile": profile, "filters": map[string]any{}})
	require.Equal(t, "CONFLICTING", out["status"])
	passages := conflictField[[]any](t, out, "passages")
	require.Len(t, passages, 2)
	for _, p := range passages {
		require.Equal(t, true, schemaMap(t, p)["compared"])
	}
	figures := conflictField[[]any](t, schemaMap(t, conflictField[[]any](t, out, "conflicts")[0]), "figures")
	quotes := map[string]string{}
	for _, f := range figures {
		figure := schemaMap(t, f)
		quotes[conflictField[string](t, figure, "value")] = conflictField[string](t, figure, "quote")
	}
	require.Equal(t, map[string]string{"15": "The cooling period is 15 days.", "30": "The cooling period is 30 days."}, quotes)
}

func TestFindConflictsSkipsRepeatedRenditionExcerpt(t *testing.T) {
	f := newNativeExportFixture(t)
	block := strings.Repeat("General policy information applies to all accounts. ", 6) + "The cooling period is 30 days. " + strings.Repeat("Contact support for help with your account. ", 6)
	body := "Premium plans.\n\n" + block + "\n\nStandard plans.\n\n" + block
	hash, size, err := f.blobs.WriteContext(t.Context(), strings.NewReader("MIME-Version: 1.0\r\nContent-Type: text/plain; charset=utf-8\r\n\r\n"+body+"\r\n"))
	require.NoError(t, err)
	node, err := f.catalog.CreateFile(t.Context(), f.catalog.RootID(), "policy.eml", hash, size, "message/rfc822")
	require.NoError(t, err)
	version, err := f.catalog.ContentVersionByID(t.Context(), node.CurrentVersionID)
	require.NoError(t, err)
	spool, err := filepath.EvalSymlinks(t.TempDir())
	require.NoError(t, err)
	_, err = processing.EnsureEmailTarget(t.Context(), f.catalog, f.blobs, spool, store.EmailTarget{Version: version})
	require.NoError(t, err)
	member := f.addFile(t, "policy.txt", "The cooling period is 15 days.")
	require.NoError(t, f.catalog.RecordExtraction(t.Context(), store.ExtractionResult{BlobHash: member.SHA256, Extractor: "plain-text", ExtractorVersion: 1, Status: store.ExtractionOK, Text: "The cooling period is 15 days."}))
	daemon, _, profile := newProcessingDocumentServer(t, f)
	search, err := daemonRead(t.Context(), daemon.daemon, func(ctx context.Context, c *daemonconn.Connection) (documentSearchResponse, error) {
		return runDocumentSearch(ctx, daemonDocumentClient{c}, searchDocumentsInput{Query: "cooling period", Profile: profile, Mode: "lexical", Filters: &api.DocumentSourceFenceFilters{}}, true, nil)
	})
	require.NoError(t, err)
	var repeatedExcerpt string
	for _, hit := range search.report.Results {
		if hit.NodeID == node.ID {
			repeatedExcerpt = strings.Trim(hit.Excerpt, " …")
		}
	}
	require.NotEmpty(t, repeatedExcerpt)
	require.Greater(t, strings.Count(body, repeatedExcerpt), 1, repeatedExcerpt)
	out := exportCall(t, daemon, "find_conflicts", map[string]any{"query": "cooling period", "profile": profile, "filters": map[string]any{}})
	require.Equal(t, "NOT_COMPARABLE", out["status"])
	require.Empty(t, out["conflicts"])
	passages := conflictField[[]any](t, out, "passages")
	require.Len(t, passages, 2)
	for _, p := range passages {
		require.Equal(t, false, schemaMap(t, p)["compared"])
	}
}

func TestFindConflictsRenditionSource(t *testing.T) {
	const sentence = "The cooling period is 30 days."
	const versionID = "11111111-1111-4111-8111-111111111111"
	attachmentID, buildID := strings.Repeat("a", 64), strings.Repeat("b", 64)
	otherAttachment, otherBuild := strings.Repeat("c", 64), strings.Repeat("d", 64)
	bodyHash := sha256.Sum256([]byte(sentence))
	rendered, _, err := document.EnvelopeRenditionV1(document.RenditionV1{
		ContractVersion: document.RenditionContractV1, Completeness: document.EvidenceComplete,
		EvidenceChecksum: buildID, Markdown: []byte(sentence), MarkdownChecksum: hex.EncodeToString(bodyHash[:]),
		Units: []document.NormalizedUnitV1{{EvidenceUnitID: "section:000000", Text: sentence,
			Locator: document.EvidenceLocatorV1{Kind: document.EvidenceLocatorSection, IndexOrigin: document.EvidenceIndexOriginNone}}},
	}, document.RenditionEnvelopeV1{BuildID: buildID, SourceSHA256: attachmentID,
		SourceFormat: "txt", SourceMediaType: "text/plain", RenditionRequestFingerprint: buildID,
		EvidenceLexicalFingerprint: buildID, NormalizedEvidenceContract: document.NormalizedEvidenceContractV1,
		UnitKind: document.EvidenceUnitSection})
	require.NoError(t, err)
	for _, tc := range []struct {
		name, text, want string
		missingFirst     bool
	}{
		{"v1 envelope", string(rendered.Markdown), sentence, false},
		{"future envelope", strings.Replace(string(rendered.Markdown), "docbank-sanitized-markdown/v1", "docbank-sanitized-markdown/v2", 1), "", false},
		{"malformed envelope", "---\ndocbank: invalid\n---\n" + sentence, "", false},
		{"email without envelope", "```\n" + sentence + "\n```", "```\n" + sentence + "\n```", false},
		{"later matching build", string(rendered.Markdown), sentence, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			digest := sha256.Sum256([]byte(tc.text))
			requests := make(chan string, 4)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests <- r.URL.Path
				id := strings.TrimPrefix(r.URL.Path, "/api/v1/renditions/")
				if r.Method != http.MethodGet || id != attachmentID {
					http.NotFound(w, r)
					return
				}
				w.Header().Set("Content-Type", "text/markdown; charset=utf-8")
				w.Header().Set(api.RenditionAttachmentHeader, id)
				w.Header().Set(api.RenditionBuildHeader, buildID)
				w.Header().Set(api.RenditionArtifactHeader, attachmentID)
				w.Header().Set(api.RenditionProfileHeader, buildID)
				w.Header().Set(api.RenditionCompletenessHeader, "complete")
				w.Header().Set(api.ContentVersionHeader, versionID)
				w.Header().Set(api.BlobHashHeader, hex.EncodeToString(digest[:]))
				w.Header().Set(api.BlobSizeHeader, strconv.Itoa(len(tc.text)))
				w.Header().Set("Trailer", "Content-Digest")
				_, _ = w.Write([]byte(tc.text))
				w.Header().Set("Content-Digest", "sha-256=:"+base64.StdEncoding.EncodeToString(digest[:])+":")
			}))
			t.Cleanup(server.Close)
			c := daemonconn.New(server.URL, "synthetic-export-key")
			t.Cleanup(func() { require.NoError(t, c.Close()) })
			hit := api.DocumentSearchResult{ContentVersionID: versionID, Evidence: []api.DocumentEvidenceReference{{BuildID: buildID}}}
			summary := api.DocumentSummary{MediaType: "text/plain", ActiveRenditions: []api.DocumentRenditionIdentity{{AttachmentID: attachmentID, BuildID: buildID}}}
			wantRequests := []string{"/api/v1/renditions/" + attachmentID}
			if tc.missingFirst {
				hit.Evidence = append([]api.DocumentEvidenceReference{{BuildID: otherBuild}}, hit.Evidence...)
				summary.ActiveRenditions = append([]api.DocumentRenditionIdentity{{AttachmentID: otherAttachment, BuildID: otherBuild}}, summary.ActiveRenditions...)
				wantRequests = append([]string{"/api/v1/renditions/" + otherAttachment}, wantRequests...)
			}
			require.Equal(t, tc.want, sourceText(t.Context(), c, hit, summary))
			require.Len(t, requests, len(wantRequests))
			for _, path := range wantRequests {
				require.Equal(t, path, <-requests)
			}
		})
	}
}

func TestFindConflictsSkipsTruncatedRendition(t *testing.T) {
	f := newNativeExportFixture(t)
	cfg, err := config.Default().WithLocalText()
	require.NoError(t, err)
	resolved, err := cfg.ProcessingProfile(config.LocalTextProfile)
	require.NoError(t, err)
	profile := resolved.Document
	profile.Embeddings = nil
	profile.EvidenceLexical.MaxUnitRunes = len(`The service fee is \$7\.`)
	profile.EvidenceLexical.MaxSegmentRunes = profile.EvidenceLexical.MaxUnitRunes
	provider, err := plaintext.New(plaintext.Profile{MaxDocumentBytes: 16 << 20})
	require.NoError(t, err)
	service, err := processing.NewService(t.Context(), processing.ServiceConfig{Catalog: f.catalog, Blobs: f.blobs, Gate: api.NewOperationGate(), SpoolDirectory: t.TempDir(), Profiles: map[string]processing.ProfileConfig{"text": {Profile: profile, RenditionProvider: provider}}})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, service.Shutdown(context.WithoutCancel(t.Context()))) })
	for i, text := range []string{"The service fee is $7.50.", "The service fee is $7.60."} {
		member := f.addFile(t, fmt.Sprintf("policy-%d.txt", i), text)
		selector := processing.Selector{NodeID: member.NodeID, ContentVersionID: member.VersionID, Profile: "text"}
		plan, err := service.Plan(t.Context(), selector)
		require.NoError(t, err)
		job, err := service.Start(t.Context(), processing.StartRequest{Selector: selector, PlanFingerprint: plan.Fingerprint, Consent: true})
		require.NoError(t, err)
		view, err := f.catalog.ActiveRendition(t.Context(), member.VersionID, job.ProfileFingerprint)
		require.NoError(t, err)
		require.Contains(t, view.Build.Warnings, "truncated")
	}
	cfg.Server.APIKey = "synthetic-export-key"
	apiServer := api.NewServer(api.Deps{Store: f.catalog, Blobs: f.blobs, Processing: service, Cfg: cfg, VaultRoot: os.Getenv("DOCBANK_HOME")})
	t.Cleanup(apiServer.Close)
	httpServer := httptest.NewServer(apiServer.Handler())
	t.Cleanup(httpServer.Close)
	daemon := newServerWithOptionsAndDaemon(testImplementation(), ServerOptions{}, exportTestLease(t, httpServer.URL))
	search, err := daemonRead(t.Context(), daemon.daemon, func(ctx context.Context, c *daemonconn.Connection) (documentSearchResponse, error) {
		return runDocumentSearch(ctx, daemonDocumentClient{c}, searchDocumentsInput{Query: "service fee", Profile: "text", Mode: "lexical", Filters: &api.DocumentSourceFenceFilters{}}, true, nil)
	})
	require.NoError(t, err)
	require.Len(t, search.report.Results, 2)
	for _, hit := range search.report.Results {
		require.Equal(t, `The service fee is \$7\.`, hit.Excerpt)
	}
	out := exportCall(t, daemon, "find_conflicts", map[string]any{"query": "service fee", "profile": "text", "filters": map[string]any{}})
	require.Equal(t, "NOT_COMPARABLE", out["status"])
	passages := conflictField[[]any](t, out, "passages")
	require.Len(t, passages, 2)
	for _, p := range passages {
		require.Equal(t, false, schemaMap(t, p)["compared"])
	}
	require.Empty(t, out["conflicts"])
}

func conflictField[T any](t *testing.T, object map[string]any, key string) T {
	t.Helper()
	value, ok := object[key].(T)
	require.True(t, ok, "%s is %T", key, object[key])
	return value
}
