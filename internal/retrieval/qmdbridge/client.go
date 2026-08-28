package qmdbridge

import (
	"bytes"
	"context"
	json "encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"slices"
	"strings"
	"unicode/utf8"

	"go.kenn.io/docbank/internal/qmdexport"
	"go.kenn.io/docbank/internal/retrieval"
	"go.kenn.io/docbank/internal/store"
)

var (
	ErrInvalidResponse = errors.New("QMD bridge response is invalid")
	ErrResponseBound   = errors.New("QMD bridge response exceeds bound")
	ErrStaleGeneration = errors.New("QMD export generation changed during query")
)

type SearchType string

const (
	SearchLexical SearchType = "lex"
	SearchVector  SearchType = "vec"
	SearchHyDE    SearchType = "hyde"
)

type Search struct {
	Type  SearchType `json:"type"`
	Query string     `json:"query"`
}

type Request struct {
	Searches []Search
	Intent   string
	Limit    int
	MinScore float64
	Rerank   *bool
	Scope    store.SearchOptions
}

type Operation struct {
	ProfileID          string
	CompatibilityEpoch string
	Collection         string
	GenerationID       string
	ManifestChecksum   string
	Scope              store.SearchOptions
	QueryCount         int
	CandidateLimit     int
	DisclosedBytes     int
}

type Result struct {
	Document         retrieval.DocumentIdentity
	NodeRevision     int64
	Path             string
	Score            float64
	Excerpt          string
	QMDURI           string
	GenerationID     string
	ManifestChecksum string
	AttachmentID     string
	BuildID          string
	ArtifactID       string
}

type wireRequest struct {
	Searches    []Search `json:"searches"`
	Limit       int      `json:"limit"`
	MinScore    float64  `json:"minScore"`
	Collections []string `json:"collections"`
	Intent      string   `json:"intent,omitempty"`
	Rerank      *bool    `json:"rerank,omitempty"`
}

type wireResponse struct {
	Results []wireResult `json:"results"`
}

type wireResult struct {
	DocID   string  `json:"docid"`
	File    string  `json:"file"`
	Title   string  `json:"title"`
	Score   float64 `json:"score"`
	Context *string `json:"context"`
	Snippet string  `json:"snippet"`
}

func (client *Client) Search(ctx context.Context, request Request) ([]Result, error) {
	if client == nil || ctx == nil {
		return nil, errors.New("qmd bridge client and context are required")
	}
	disclosed, err := client.validateRequest(request)
	if err != nil {
		return nil, err
	}
	normalizedScope, err := client.authority.NormalizeQMDSearchScope(ctx, request.Scope)
	if err != nil {
		return nil, fmt.Errorf("normalize QMD search scope: %w", err)
	}
	before, err := qmdexport.LoadCurrent(client.root)
	if err != nil {
		return nil, fmt.Errorf("load active QMD export: %w", err)
	}
	payload, err := json.Marshal(wireRequest{Searches: slices.Clone(request.Searches), Limit: request.Limit,
		MinScore: request.MinScore, Collections: []string{before.Manifest.Collection}, Intent: request.Intent,
		Rerank: request.Rerank}, json.Deterministic(true))
	if err != nil || int64(len(payload)) > client.profile.MaxRequestBytes {
		return nil, errors.New("qmd bridge request exceeds bound")
	}
	defer clear(payload)
	requestCtx, cancel := context.WithTimeout(ctx, client.profile.RequestTimeout)
	defer cancel()
	secret, err := client.secrets.ResolveSecret(requestCtx, client.profile.SecretBinding)
	if err != nil || !validToken(secret) {
		return nil, errors.New("qmd bridge named authentication resolution failed")
	}
	operation := Operation{ProfileID: client.profile.ID, CompatibilityEpoch: client.profile.CompatibilityEpoch,
		Collection: before.Manifest.Collection, GenerationID: before.GenerationID,
		ManifestChecksum: before.Manifest.Checksum, Scope: normalizedScope, QueryCount: len(request.Searches),
		CandidateLimit: request.Limit, DisclosedBytes: disclosed}
	if err := client.authorizer.AuthorizeQMDQuery(requestCtx, operation); err != nil {
		return nil, fmt.Errorf("authorize QMD query: %w", err)
	}
	if err := requestCtx.Err(); err != nil {
		return nil, err
	}
	httpRequest, err := http.NewRequestWithContext(requestCtx, http.MethodPost, client.endpoint, bytes.NewReader(payload))
	if err != nil {
		return nil, errors.New("qmd bridge request construction failed")
	}
	httpRequest.Header.Set("Accept", "application/json")
	httpRequest.Header.Set("Content-Type", "application/json")
	httpRequest.Header.Set("Authorization", "Bearer "+secret)
	response, err := client.http.Do(httpRequest)
	if err != nil {
		if requestCtx.Err() != nil {
			return nil, fmt.Errorf("qmd bridge request canceled: %w", requestCtx.Err())
		}
		return nil, errors.New("qmd bridge request failed")
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusOK || !jsonContentType(response.Header.Get("Content-Type")) {
		return nil, ErrInvalidResponse
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, client.profile.MaxResponseBytes+1))
	defer clear(body)
	if err != nil {
		if requestCtx.Err() != nil {
			return nil, fmt.Errorf("qmd bridge response read canceled: %w", requestCtx.Err())
		}
		return nil, errors.New("qmd bridge response read failed")
	}
	if int64(len(body)) > client.profile.MaxResponseBytes {
		return nil, ErrResponseBound
	}
	var decoded wireResponse
	if err := json.Unmarshal(body, &decoded, json.RejectUnknownMembers(true)); err != nil {
		return nil, ErrInvalidResponse
	}
	mapped, entries, err := client.mapResults(decoded.Results, before, request.Limit)
	if err != nil {
		return nil, err
	}
	after, err := qmdexport.LoadCurrent(client.root)
	if err != nil || after.GenerationID != before.GenerationID || after.Manifest.Checksum != before.Manifest.Checksum {
		return nil, ErrStaleGeneration
	}
	live, err := client.authority.RevalidateQMDExportCandidates(requestCtx, entries, normalizedScope)
	if err != nil {
		return nil, fmt.Errorf("revalidate QMD result authority: %w", err)
	}
	if len(live) != len(mapped) {
		return nil, store.ErrQMDExportAuthorityStale
	}
	final, err := qmdexport.LoadCurrent(client.root)
	if err != nil || final.GenerationID != before.GenerationID || final.Manifest.Checksum != before.Manifest.Checksum {
		return nil, ErrStaleGeneration
	}
	results := make([]Result, len(mapped))
	for index := range mapped {
		if live[index].NodeID != entries[index].NodeID || live[index].ContentVersionID != entries[index].ContentVersionID {
			return nil, store.ErrQMDExportAuthorityStale
		}
		results[index] = Result{Document: retrieval.DocumentIdentity{VaultID: entries[index].VaultUID,
			NodeID: live[index].NodeID, ContentVersionID: live[index].ContentVersionID},
			NodeRevision: live[index].NodeRevision, Path: live[index].Path, Score: mapped[index].Score,
			Excerpt: mapped[index].Snippet, QMDURI: mapped[index].File, GenerationID: before.GenerationID,
			ManifestChecksum: before.Manifest.Checksum, AttachmentID: entries[index].AttachmentID,
			BuildID: entries[index].BuildID, ArtifactID: entries[index].ArtifactID}
	}
	return results, nil
}

func (client *Client) validateRequest(request Request) (int, error) {
	if len(request.Searches) < 1 || len(request.Searches) > maximumQueries ||
		request.Limit < 1 || request.Limit > client.profile.MaxCandidates || !finiteUnit(request.MinScore) ||
		!utf8.ValidString(request.Intent) || len(request.Intent) > client.profile.MaxIntentBytes {
		return 0, errors.New("qmd bridge request is invalid")
	}
	total := len(request.Intent)
	for _, search := range request.Searches {
		if search.Type != SearchLexical && search.Type != SearchVector && search.Type != SearchHyDE ||
			strings.TrimSpace(search.Query) == "" || !utf8.ValidString(search.Query) ||
			len(search.Query) > client.profile.MaxQueryBytes-total {
			return 0, errors.New("qmd bridge request is invalid")
		}
		total += len(search.Query)
	}
	return total, nil
}

func (client *Client) mapResults(items []wireResult, snapshot qmdexport.Receipt, limit int) ([]wireResult, []store.QMDExportSource, error) {
	if len(items) > limit || len(items) > client.profile.MaxCandidates {
		return nil, nil, ErrInvalidResponse
	}
	manifest := make(map[string]qmdexport.Entry, len(snapshot.Manifest.Entries))
	for _, entry := range snapshot.Manifest.Entries {
		manifest[entry.URI] = entry
	}
	seenURI := make(map[string]struct{}, len(items))
	seenNode := make(map[int64]struct{}, len(items))
	entries := make([]store.QMDExportSource, len(items))
	for index, item := range items {
		entry, exists := manifest[item.File]
		if !exists || !validWireResult(item, client.profile.MaxSnippetBytes) {
			return nil, nil, ErrInvalidResponse
		}
		if _, duplicate := seenURI[item.File]; duplicate {
			return nil, nil, ErrInvalidResponse
		}
		if _, duplicate := seenNode[entry.NodeID]; duplicate {
			return nil, nil, ErrInvalidResponse
		}
		seenURI[item.File], seenNode[entry.NodeID] = struct{}{}, struct{}{}
		entries[index] = store.QMDExportSource{VaultUID: entry.VaultUID, NodeID: entry.NodeID,
			ContentVersionID: entry.ContentVersionID, ProcessingProfileFingerprint: entry.ProcessingProfileFingerprint,
			AttachmentID: entry.AttachmentID, BuildID: entry.BuildID, ArtifactID: entry.ArtifactID,
			BlobSHA256: entry.BlobSHA256, BlobSize: entry.BlobSize,
			ArtifactChecksum: entry.ArtifactChecksum, MarkdownChecksum: entry.MarkdownChecksum}
	}
	return items, entries, nil
}

func validWireResult(item wireResult, maxSnippet int) bool {
	if !validToken(item.DocID) || item.File == "" || len(item.File) > 2048 ||
		!utf8.ValidString(item.File) || len(item.Title) > 4096 || !utf8.ValidString(item.Title) ||
		len(item.Snippet) > maxSnippet || !utf8.ValidString(item.Snippet) || !finiteUnit(item.Score) {
		return false
	}
	return item.Context == nil || len(*item.Context) <= 4096 && utf8.ValidString(*item.Context)
}

func jsonContentType(value string) bool {
	mediaType, parameters, err := mime.ParseMediaType(value)
	if err != nil || mediaType != "application/json" {
		return false
	}
	charset, ok := parameters["charset"]
	return len(parameters) == 0 || len(parameters) == 1 && ok && strings.EqualFold(charset, "utf-8")
}
