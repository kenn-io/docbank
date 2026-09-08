package qmdbridge

import (
	"bytes"
	"context"
	json "encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"net/http"
	"path"
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

func (client *Client) Search(ctx context.Context, request Request) ([]Result, error) {
	if client == nil || ctx == nil {
		return nil, errors.New("qmd bridge client and context are required")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	requestCtx, cancel := context.WithTimeout(ctx, client.profile.RequestTimeout)
	defer cancel()
	if len(request.Searches) < 1 || len(request.Searches) > maximumQueries {
		return nil, errors.New("qmd bridge request is invalid")
	}
	if len(request.Scope.ContentVersionIDs) > store.MaxSearchSourceFenceIDs {
		return nil, errors.New("QMD source fence exceeds 4096 content versions")
	}
	owned := request
	owned.Searches = slices.Clone(request.Searches)
	owned.Scope = cloneQMDScope(request.Scope)
	if request.Rerank != nil {
		value := *request.Rerank
		owned.Rerank = &value
	}
	disclosed, err := client.validateRequest(owned)
	if err != nil {
		return nil, err
	}
	if err := requestCtx.Err(); err != nil {
		return nil, err
	}

	normalizedScope, err := client.authority.NormalizeQMDSearchScope(requestCtx, cloneQMDScope(owned.Scope))
	if err != nil {
		return nil, errors.Join(fmt.Errorf("normalize QMD search scope: %w", err), requestCtx.Err())
	}
	if err := requestCtx.Err(); err != nil {
		return nil, err
	}
	if len(normalizedScope.ContentVersionIDs) > store.MaxSearchSourceFenceIDs {
		return nil, errors.New("QMD source fence exceeds 4096 content versions")
	}
	normalizedScope = cloneQMDScope(normalizedScope)
	before, err := qmdexport.LoadCurrent(client.root)
	if err != nil {
		return nil, errors.Join(fmt.Errorf("load active QMD export: %w", err), requestCtx.Err())
	}
	if err := requestCtx.Err(); err != nil {
		return nil, err
	}
	payload, err := json.Marshal(wireRequest{Searches: owned.Searches, Limit: owned.Limit, MinScore: owned.MinScore,
		Collections: []string{before.Manifest.Collection}, Intent: owned.Intent, Rerank: owned.Rerank}, json.Deterministic(true))
	if err != nil || int64(len(payload)) > client.profile.MaxRequestBytes {
		return nil, errors.New("qmd bridge request exceeds bound")
	}
	defer clear(payload)
	secret, err := client.secrets.ResolveSecret(requestCtx, client.profile.SecretBinding)
	if err != nil {
		return nil, errors.Join(errors.New("qmd bridge named authentication resolution failed"), requestCtx.Err())
	}
	if err := requestCtx.Err(); err != nil {
		return nil, err
	}
	if !validToken(secret) {
		return nil, errors.New("qmd bridge named authentication resolution failed")
	}
	operation := Operation{ProfileID: client.profile.ID, CompatibilityEpoch: client.profile.CompatibilityEpoch,
		Collection: before.Manifest.Collection, GenerationID: before.GenerationID, ManifestChecksum: before.Manifest.Checksum,
		Scope: cloneQMDScope(normalizedScope), QueryCount: len(owned.Searches), CandidateLimit: owned.Limit, DisclosedBytes: disclosed}
	if err := client.authorizer.AuthorizeQMDQuery(requestCtx, operation); err != nil {
		return nil, errors.Join(fmt.Errorf("authorize QMD query: %w", err), requestCtx.Err())
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
	if err := requestCtx.Err(); err != nil {
		return nil, err
	}
	if response.StatusCode != http.StatusOK || !jsonContentType(response.Header.Get("Content-Type")) {
		return nil, ErrInvalidResponse
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, client.profile.MaxResponseBytes+1))
	defer clear(body)
	if err != nil {
		return nil, errors.Join(errors.New("qmd bridge response read failed"), err, requestCtx.Err())
	}
	if err := requestCtx.Err(); err != nil {
		return nil, err
	}
	if int64(len(body)) > client.profile.MaxResponseBytes {
		return nil, ErrResponseBound
	}
	items, err := decodeResponse(body, owned.Limit)
	if err != nil {
		return nil, err
	}
	mapped, entries, err := client.mapResults(items, before)
	if err != nil {
		return nil, err
	}
	if err := requestCtx.Err(); err != nil {
		return nil, err
	}
	if err := client.requireCurrent(requestCtx, before); err != nil {
		return nil, err
	}
	live, err := client.authority.RevalidateQMDExportCandidates(requestCtx, slices.Clone(entries), cloneQMDScope(normalizedScope))
	if err != nil {
		return nil, errors.Join(fmt.Errorf("revalidate QMD result authority: %w", err), requestCtx.Err())
	}
	if err := requestCtx.Err(); err != nil {
		return nil, err
	}
	if err := validateLive(entries, live); err != nil {
		return nil, err
	}
	if err := client.requireCurrent(requestCtx, before); err != nil {
		return nil, err
	}
	results := make([]Result, len(mapped))
	for index := range mapped {
		entry, item := entries[index], mapped[index]
		results[index] = Result{Document: retrieval.DocumentIdentity{VaultID: entry.VaultUID, NodeID: live[index].NodeID,
			ContentVersionID: live[index].ContentVersionID}, NodeRevision: live[index].NodeRevision, Path: live[index].Path,
			Score: *item.Score, Excerpt: item.Snippet, QMDURI: item.File, GenerationID: before.GenerationID,
			ManifestChecksum: before.Manifest.Checksum, AttachmentID: entry.AttachmentID, BuildID: entry.BuildID, ArtifactID: entry.ArtifactID}
	}
	if err := requestCtx.Err(); err != nil {
		return nil, err
	}
	return results, nil
}

func cloneQMDScope(scope store.SearchOptions) store.SearchOptions {
	scope.ContentVersionIDs = slices.Clone(scope.ContentVersionIDs)
	return scope
}

func (client *Client) requireCurrent(ctx context.Context, before qmdexport.Receipt) error {
	current, err := qmdexport.LoadCurrent(client.root)
	if err != nil {
		return errors.Join(ErrStaleGeneration, err, ctx.Err())
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if current.GenerationID != before.GenerationID || current.Manifest.Checksum != before.Manifest.Checksum {
		return ErrStaleGeneration
	}
	return nil
}

func (client *Client) validateRequest(request Request) (int, error) {
	if len(request.Searches) < 1 || len(request.Searches) > maximumQueries || request.Limit < 1 ||
		request.Limit > client.profile.MaxCandidates || !finiteUnit(request.MinScore) || !utf8.ValidString(request.Intent) ||
		len(request.Intent) > client.profile.MaxIntentBytes || len(request.Intent) > client.profile.MaxQueryBytes {
		return 0, errors.New("qmd bridge request is invalid")
	}
	total := len(request.Intent)
	for _, search := range request.Searches {
		if search.Type != SearchLexical && search.Type != SearchVector && search.Type != SearchHyDE ||
			strings.TrimSpace(search.Query) == "" || !utf8.ValidString(search.Query) || len(search.Query) > client.profile.MaxQueryBytes-total {
			return 0, errors.New("qmd bridge request is invalid")
		}
		total += len(search.Query)
	}
	return total, nil
}

func (client *Client) mapResults(items []wireResult, snapshot qmdexport.Receipt) ([]wireResult, []store.QMDExportSource, error) {
	manifest := make(map[string]qmdexport.Entry, len(snapshot.Manifest.Entries))
	for _, entry := range snapshot.Manifest.Entries {
		manifest[entry.URI] = entry
	}
	seenURI := make(map[string]struct{}, len(items))
	entries := make([]store.QMDExportSource, len(items))
	for index, item := range items {
		entry, exists := manifest[item.File]
		if !exists || !validWireResult(item, client.profile.MaxSnippetBytes) {
			return nil, nil, ErrInvalidResponse
		}
		if _, duplicate := seenURI[item.File]; duplicate {
			return nil, nil, ErrInvalidResponse
		}
		seenURI[item.File] = struct{}{}
		entries[index] = store.QMDExportSource{VaultUID: entry.VaultUID, NodeID: entry.NodeID, ContentVersionID: entry.ContentVersionID,
			ProcessingProfileFingerprint: entry.ProcessingProfileFingerprint, AttachmentID: entry.AttachmentID, BuildID: entry.BuildID,
			ArtifactID: entry.ArtifactID, BlobSHA256: entry.BlobSHA256, BlobSize: entry.BlobSize,
			ArtifactChecksum: entry.ArtifactChecksum, MarkdownChecksum: entry.MarkdownChecksum}
	}
	return items, entries, nil
}

func validateLive(entries []store.QMDExportSource, live []store.QMDExportLiveCandidate) error {
	if len(live) != len(entries) {
		return store.ErrQMDExportAuthorityStale
	}
	for index, candidate := range live {
		entry := entries[index] // #nosec G602 -- exact slice lengths are compared above.
		if candidate.NodeID != entry.NodeID || candidate.ContentVersionID != entry.ContentVersionID ||
			candidate.NodeRevision < 1 || !validLogicalPath(candidate.Path) {
			return store.ErrQMDExportAuthorityStale
		}
	}
	return nil
}

func validLogicalPath(value string) bool {
	return value != "" && utf8.ValidString(value) && strings.HasPrefix(value, "/") && !strings.Contains(value, "\\") &&
		!strings.ContainsRune(value, 0) && path.Clean(value) == value
}
