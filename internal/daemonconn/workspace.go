package daemonconn

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"go.kenn.io/docbank/internal/apiclient"
	"strconv"
	"strings"

	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/query"
	"go.kenn.io/docbank/internal/store"
)

// CreateWorkspaceQuery opens one exact daemon-owned query snapshot and
// returns its first bounded page.
func (c *Connection) CreateWorkspaceQuery(
	ctx context.Context, request api.WorkspaceQueryCreateRequest,
) (api.WorkspaceQueryResponse, error) {
	var response api.WorkspaceQueryResponse
	if request.FacetsOnly {
		return response, errors.New("counts requests do not return snapshots")
	}
	if _, err := validateWorkspaceRequest(request.Query, request.PageSize, request.Facets); err != nil {
		return response, err
	}
	apiResponse, err := c.API().CreateWorkspaceQuery(ctx, &apiclient.CreateWorkspaceQueryRequestOptions{Body: &request})
	if err != nil {
		return api.WorkspaceQueryResponse{}, err
	}
	if apiResponse.Snapshot == nil {
		return response, errors.New("workspace response lacks snapshot authority")
	}
	response = *apiResponse.Snapshot
	if err := validateWorkspaceQueryResponse(response); err != nil {
		return api.WorkspaceQueryResponse{}, err
	}
	return response, nil
}

// ReadWorkspaceQueryPage reads an immutable page from an existing snapshot.
// A missing handle is returned as store.ErrSnapshotGone; it is never rerun.
func (c *Connection) ReadWorkspaceQueryPage(
	ctx context.Context, snapshotID, cursor string,
) (api.WorkspaceQueryResponse, error) {
	var response api.WorkspaceQueryResponse
	if !validSnapshotID(snapshotID) {
		return response, errors.New("snapshot ID must be 32 lowercase hexadecimal characters")
	}
	if cursor == "" || len(cursor) > 2048 {
		return response, errors.New("snapshot cursor must contain 1 through 2048 bytes")
	}
	apiResponse, err := c.API().ReadWorkspaceQueryPage(ctx, &apiclient.ReadWorkspaceQueryPageRequestOptions{PathParams: &apiclient.ReadWorkspaceQueryPagePath{ID: snapshotID}, Body: &api.WorkspaceQueryPageRequest{Cursor: cursor}})
	if err != nil {
		return api.WorkspaceQueryResponse{}, err
	}
	response = *apiResponse
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
func (c *Connection) RunSavedQuery(
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
	apiResponse, err := c.API().RunSavedQuery(ctx, &apiclient.RunSavedQueryRequestOptions{PathParams: &apiclient.RunSavedQueryPath{SavedQueryID: id}, Header: &apiclient.RunSavedQueryHeaders{IfMatch: strconv.Quote(strconv.FormatInt(revision, 10))}, Body: &request})
	if err != nil {
		return api.SavedQueryRunResult{}, err
	}
	result = *apiResponse
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
	_, err := store.NormalizeSnapshotRequest(store.SnapshotRequest{PageSize: pageSize, Facets: facets})
	return err
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
