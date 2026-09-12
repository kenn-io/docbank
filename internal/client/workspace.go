package client

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/query"
)

// CreateWorkspaceQuery opens one exact daemon-owned query snapshot and
// returns its first bounded page.
func (c *Client) CreateWorkspaceQuery(
	ctx context.Context, request api.WorkspaceQueryCreateRequest,
) (api.WorkspaceQueryResponse, error) {
	var response api.WorkspaceQueryResponse
	if _, err := validateWorkspaceRequest(request.Query, request.PageSize, request.Facets); err != nil {
		return response, err
	}
	if err := c.do(ctx, http.MethodPost, "/api/v1/workspace/queries", nil, request, &response); err != nil {
		return api.WorkspaceQueryResponse{}, err
	}
	if err := validateWorkspaceQueryResponse(response); err != nil {
		return api.WorkspaceQueryResponse{}, err
	}
	return response, nil
}

// ReadWorkspaceQueryPage reads an immutable page from an existing snapshot.
// A missing handle is returned as store.ErrSnapshotGone; it is never rerun.
func (c *Client) ReadWorkspaceQueryPage(
	ctx context.Context, snapshotID, cursor string,
) (api.WorkspaceQueryResponse, error) {
	var response api.WorkspaceQueryResponse
	if !validSnapshotID(snapshotID) {
		return response, errors.New("snapshot ID must be 32 lowercase hexadecimal characters")
	}
	if cursor == "" || len(cursor) > 2048 {
		return response, errors.New("snapshot cursor must contain 1 through 2048 bytes")
	}
	path := "/api/v1/workspace/queries/" + url.PathEscape(snapshotID) + "/pages"
	if err := c.do(ctx, http.MethodPost, path, nil, api.WorkspaceQueryPageRequest{Cursor: cursor}, &response); err != nil {
		return api.WorkspaceQueryResponse{}, err
	}
	if err := validateWorkspaceQueryResponse(response); err != nil {
		return api.WorkspaceQueryResponse{}, err
	}
	if response.SnapshotID != snapshotID {
		return api.WorkspaceQueryResponse{}, errors.New("workspace page does not match requested snapshot")
	}
	return response, nil
}

// RunSavedQuery executes exactly one inspected saved-definition revision and
// returns its durable receipt with the first ephemeral snapshot page.
func (c *Client) RunSavedQuery(
	ctx context.Context, id string, revision int64, request api.SavedQueryRunRequest,
) (api.SavedQueryRunResult, error) {
	var result api.SavedQueryRunResult
	if !validUUIDv4(id) {
		return result, errors.New("saved query ID must be a canonical UUIDv4")
	}
	if revision < 1 {
		return result, errors.New("saved query revision must be positive")
	}
	if err := validateWorkspaceOptions(request.PageSize, request.Facets); err != nil {
		return result, err
	}
	path := "/api/v1/saved-queries/" + url.PathEscape(id) + "/runs"
	if err := c.do(ctx, http.MethodPost, path, ifMatch(revision), request, &result); err != nil {
		return api.SavedQueryRunResult{}, err
	}
	if err := validateWorkspaceQueryResponse(result.Snapshot); err != nil {
		return api.SavedQueryRunResult{}, fmt.Errorf("saved query run snapshot: %w", err)
	}
	if err := validateSavedQueryRunResult(result, id, revision); err != nil {
		return api.SavedQueryRunResult{}, err
	}
	return result, nil
}

func validateWorkspaceRequest(raw api.QueryPayload, pageSize int, facets []string) (query.Query, error) {
	value, err := query.Parse(raw)
	if err != nil {
		return query.Query{}, fmt.Errorf("invalid workspace query: %w", err)
	}
	if err := validateWorkspaceOptions(pageSize, facets); err != nil {
		return query.Query{}, err
	}
	return value, nil
}

func validateWorkspaceOptions(pageSize int, facets []string) error {
	if pageSize != 0 && pageSize != 50 && pageSize != 100 && pageSize != 250 {
		return errors.New("workspace page size must be 50, 100, or 250")
	}
	known := map[string]bool{
		"collections": true, "tags": true, "media_family": true, "extension": true,
		"modified": true, "size": true, "text_coverage": true, "duplicates": true,
	}
	seen := make(map[string]bool, len(facets))
	for _, facet := range facets {
		if !known[facet] || seen[facet] {
			return errors.New("workspace facets contain an unknown or duplicate dimension")
		}
		seen[facet] = true
	}
	return nil
}

func validateWorkspaceQueryResponse(response api.WorkspaceQueryResponse) error {
	if !response.Snapshot || !validSnapshotID(response.SnapshotID) ||
		!validPrefixedSHA256(response.QueryFingerprint) || !validSHA256Hex(response.MemberHash) ||
		!validPrefixedSHA256(response.SnapshotFingerprint) {
		return errors.New("workspace response lacks complete snapshot authority")
	}
	value, err := query.Parse(response.Query)
	if err != nil {
		return fmt.Errorf("workspace response query: %w", err)
	}
	fingerprint, err := query.Fingerprint(value)
	if err != nil || fingerprint != response.QueryFingerprint {
		return errors.New("workspace response query fingerprint is inconsistent")
	}
	if err := validateWorkspaceOptions(response.PageSize, nil); err != nil || response.PageSize == 0 ||
		response.Total < 0 || response.TotalBytes < 0 || len(response.Rows) > response.PageSize ||
		int64(len(response.Rows)) > response.Total || response.CreatedAt.IsZero() ||
		response.ObservedAt.IsZero() || response.ExpiresAt.Before(response.CreatedAt) ||
		len(response.PreviousCursor) > 2048 || len(response.NextCursor) > 2048 {
		return errors.New("workspace response has inconsistent bounds")
	}
	if response.Generation.Kind != "native" && response.Generation.Kind != "rendition" {
		return errors.New("workspace response has invalid generation evidence")
	}
	if response.Generation.Kind == "native" && response.Generation.GenerationID != "" {
		return errors.New("workspace native generation invents rendition authority")
	}
	for index, dependency := range response.Dependencies {
		if dependency.Kind == "" || dependency.ID == "" || dependency.Revision < 1 {
			return fmt.Errorf("workspace response dependency %d is invalid", index)
		}
	}
	for index, row := range response.Rows {
		if row.NodeID < 1 || !validUUIDv4(row.ContentVersionID) || !validSHA256Hex(row.BlobHash) ||
			row.Size < 0 || row.Revision < 1 || row.Path == "" || row.Name == "" {
			return fmt.Errorf("workspace response row %d is invalid", index)
		}
	}
	for index, facet := range response.Facets {
		if facet.Available {
			if facet.Reason != "" || facet.Total == nil || facet.Missing == nil || facet.Other == nil {
				return fmt.Errorf("workspace response facet %d lacks available counts", index)
			}
		} else if facet.Reason == "" || facet.Total != nil || facet.Missing != nil || facet.Other != nil || len(facet.Values) != 0 {
			return fmt.Errorf("workspace response facet %d fabricates unavailable counts", index)
		}
	}
	return nil
}

func validateSavedQueryRunResult(result api.SavedQueryRunResult, id string, revision int64) error {
	run := result.Run
	if !validUUIDv4(run.RunID) || run.SavedQueryID != id || run.SavedQueryRevision != revision ||
		run.SnapshotID != result.Snapshot.SnapshotID || run.MemberHash != result.Snapshot.MemberHash ||
		run.QueryFingerprint != result.Snapshot.QueryFingerprint || run.Total != result.Snapshot.Total ||
		run.TotalBytes != result.Snapshot.TotalBytes || run.RanAt.IsZero() || run.ExpiresAt.Before(run.RanAt) {
		return errors.New("saved query run response has inconsistent authority")
	}
	return nil
}

func validSnapshotID(value string) bool {
	if len(value) != 32 || value != strings.ToLower(value) {
		return false
	}
	decoded, err := hex.DecodeString(value)
	return err == nil && len(decoded) == 16
}

func validPrefixedSHA256(value string) bool {
	return strings.HasPrefix(value, "sha256:") && validSHA256Hex(strings.TrimPrefix(value, "sha256:"))
}
