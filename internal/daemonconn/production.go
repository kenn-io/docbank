package daemonconn

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"strconv"

	"go.kenn.io/docbank/document/redaction"
	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/apiclient"
	"go.kenn.io/docbank/internal/canonical"
	"go.kenn.io/docbank/internal/store"
	"uuid"
)

func (c *Connection) ProductionRecipes(ctx context.Context) (api.ProductionRecipeCatalog, error) {
	catalog, err := c.API().ListProductionRecipes(ctx)
	if err != nil {
		return api.ProductionRecipeCatalog{}, err
	}
	if catalog == nil || catalog.DefaultID != redaction.DefaultRecipeID || len(catalog.Items) != 2 {
		return api.ProductionRecipeCatalog{}, integrityErrorf("production recipe catalog is inconsistent")
	}
	for index, expected := range []struct {
		id  string
		dpi int
	}{{redaction.RecipeID300DPI, 300}, {redaction.RecipeID600DPI, 600}} {
		item := catalog.Items[index]
		if item.ID != expected.id || item.Recipe.DPI != expected.dpi {
			return api.ProductionRecipeCatalog{}, integrityErrorf("production recipe choice is inconsistent")
		}
		encoded, err := canonical.Marshal(redaction.Recipe(item.Recipe))
		if err != nil {
			return api.ProductionRecipeCatalog{}, integrityErrorf("production recipe cannot be verified")
		}
		digest := sha256.Sum256(encoded)
		if item.SHA256 != hex.EncodeToString(digest[:]) {
			return api.ProductionRecipeCatalog{}, integrityErrorf("production recipe digest is inconsistent")
		}
	}
	return *catalog, nil
}

func (c *Connection) CreateProductionSet(ctx context.Context, request redaction.CreateRequest) (api.ProductionSetCreated, error) {
	if err := redaction.ValidateCreateRequest(request); err != nil {
		return api.ProductionSetCreated{}, err
	}
	result, err := c.API().CreateProductionSet(ctx, &apiclient.CreateProductionSetRequestOptions{Body: &request})
	if err != nil {
		return api.ProductionSetCreated{}, err
	}
	if result == nil || redaction.ValidateSet(result.Set) != nil || redaction.ValidateDraft(result.Draft) != nil ||
		result.Set.ID != result.Draft.SetID || result.Set.Name != request.Name {
		return api.ProductionSetCreated{}, integrityErrorf("production set receipt is inconsistent")
	}
	return *result, nil
}

func (c *Connection) ProductionSet(ctx context.Context, setID string) (redaction.Set, error) {
	parsed, err := productionSetUUID(setID)
	if err != nil {
		return redaction.Set{}, err
	}
	result, err := c.API().GetProductionSet(ctx, &apiclient.GetProductionSetRequestOptions{
		PathParams: &apiclient.GetProductionSetPath{SetID: parsed}})
	if err != nil {
		return redaction.Set{}, err
	}
	if result == nil || redaction.ValidateSet(*result) != nil || result.ID != setID {
		return redaction.Set{}, integrityErrorf("production set response is inconsistent")
	}
	return *result, nil
}

func (c *Connection) ProductionSets(ctx context.Context, cursor string, limit int) (api.ProductionSetPage, error) {
	if len(cursor) > 2048 || limit < 0 || limit > redaction.MaxProductionPage {
		return api.ProductionSetPage{}, errors.New("invalid production set page")
	}
	query := &apiclient.ListProductionSetsQuery{}
	if cursor != "" {
		query.Cursor = &cursor
	}
	if limit != 0 {
		bounded := int64(limit)
		query.Limit = &bounded
	} else {
		limit = 100
	}
	result, err := c.API().ListProductionSets(ctx, &apiclient.ListProductionSetsRequestOptions{Query: query})
	if err != nil {
		return api.ProductionSetPage{}, err
	}
	if result == nil || len(result.Items) > limit || len(result.NextCursor) > 2048 ||
		result.NextCursor != "" && (len(result.Items) != limit || result.NextCursor == cursor) {
		return api.ProductionSetPage{}, integrityErrorf("production set page is inconsistent")
	}
	previous := ""
	for _, set := range result.Items {
		if redaction.ValidateSet(set) != nil || set.ID <= previous {
			return api.ProductionSetPage{}, integrityErrorf("production set page contains invalid authority")
		}
		previous = set.ID
	}
	return *result, nil
}

func (c *Connection) ProductionDraft(ctx context.Context, setID string, revision int64) (redaction.Draft, error) {
	parsed, err := productionSetUUID(setID)
	if err != nil || revision < 1 {
		return redaction.Draft{}, errors.New("invalid production revision")
	}
	result, err := c.API().GetProductionDraft(ctx, &apiclient.GetProductionDraftRequestOptions{
		PathParams: &apiclient.GetProductionDraftPath{SetID: parsed, Revision: revision}})
	if err != nil {
		return redaction.Draft{}, err
	}
	if result == nil || redaction.ValidateDraft(*result) != nil || result.SetID != setID || result.Revision != revision {
		return redaction.Draft{}, integrityErrorf("production draft response is inconsistent")
	}
	return *result, nil
}

func (c *Connection) ForkProductionDraft(ctx context.Context, setID string, revision int64,
	request api.ProductionForkRequest) (redaction.Draft, error) {
	parsed, err := productionSetUUID(setID)
	if err != nil || revision < 1 || !validUUIDv4(request.OperationID) {
		return redaction.Draft{}, errors.New("invalid production draft fork")
	}
	result, err := c.API().ForkProductionDraft(ctx, &apiclient.ForkProductionDraftRequestOptions{
		PathParams: &apiclient.ForkProductionDraftPath{SetID: parsed, Revision: revision}, Body: &request})
	if err != nil {
		return redaction.Draft{}, err
	}
	if result == nil || redaction.ValidateDraft(*result) != nil || result.SetID != setID ||
		result.Revision <= revision || result.ETag != 1 || result.State != "draft" || result.MembershipSealed {
		return redaction.Draft{}, integrityErrorf("production fork response is inconsistent")
	}
	return *result, nil
}

func (c *Connection) ProductionMembers(ctx context.Context, setID string, revision int64, cursor string, limit int) (api.ProductionMemberPage, error) {
	parsed, err := productionSetUUID(setID)
	if err != nil || revision < 1 || limit < 0 || limit > redaction.MaxProductionPage {
		return api.ProductionMemberPage{}, errors.New("invalid production member page")
	}
	query := &apiclient.ListProductionMembersQuery{}
	if cursor != "" {
		query.Cursor = &cursor
	}
	if limit != 0 {
		bounded := int64(limit)
		query.Limit = &bounded
	} else {
		limit = 100
	}
	result, err := c.API().ListProductionMembers(ctx, &apiclient.ListProductionMembersRequestOptions{
		PathParams: &apiclient.ListProductionMembersPath{SetID: parsed, Revision: revision}, Query: query})
	if err != nil {
		return api.ProductionMemberPage{}, err
	}
	if result == nil || len(result.Items) > limit {
		return api.ProductionMemberPage{}, integrityErrorf("production member page is inconsistent")
	}
	for _, item := range result.Items {
		if redaction.ValidateMember(redaction.Member(item)) != nil {
			return api.ProductionMemberPage{}, integrityErrorf("production member page contains invalid authority")
		}
	}
	return *result, nil
}

func (c *Connection) ProductionDecisions(ctx context.Context, setID string, revision int64, cursor string, limit int) (api.ProductionDecisionPage, error) {
	parsed, err := productionSetUUID(setID)
	if err != nil || revision < 1 || limit < 0 || limit > redaction.MaxProductionPage {
		return api.ProductionDecisionPage{}, errors.New("invalid production decision page")
	}
	query := &apiclient.ListProductionDecisionsQuery{}
	if cursor != "" {
		query.Cursor = &cursor
	}
	if limit != 0 {
		bounded := int64(limit)
		query.Limit = &bounded
	} else {
		limit = 100
	}
	result, err := c.API().ListProductionDecisions(ctx, &apiclient.ListProductionDecisionsRequestOptions{
		PathParams: &apiclient.ListProductionDecisionsPath{SetID: parsed, Revision: revision}, Query: query})
	if err != nil {
		return api.ProductionDecisionPage{}, err
	}
	if result == nil || len(result.Items) > limit {
		return api.ProductionDecisionPage{}, integrityErrorf("production decision page is inconsistent")
	}
	for _, item := range result.Items {
		if redaction.ValidateDecision(redaction.Decision(item)) != nil {
			return api.ProductionDecisionPage{}, integrityErrorf("production decision page contains invalid authority")
		}
	}
	return *result, nil
}

// ProductionMapChunk verifies the page digest; callers must also verify the
// assembled canonical map against MapSHA256 before using selectors from it.
func (c *Connection) ProductionMapChunk(ctx context.Context, setID string, revision int64, memberID, cursor string, limit int) (api.ProductionMapChunk, error) {
	parsedSet, err := productionSetUUID(setID)
	if err != nil || !validUUIDv4(memberID) || revision < 1 || len(cursor) > 512 ||
		limit < 0 || limit > store.MaxProductionMapChunkBytes {
		return api.ProductionMapChunk{}, errors.New("invalid production map page")
	}
	parsedMember, err := uuid.Parse(memberID)
	if err != nil {
		return api.ProductionMapChunk{}, errors.New("invalid production member ID")
	}
	query := &apiclient.GetProductionMapChunkQuery{}
	if cursor != "" {
		query.Cursor = &cursor
	}
	if limit == 0 {
		limit = store.MaxProductionMapChunkBytes
	} else {
		bounded := int64(limit)
		query.Limit = &bounded
	}
	result, err := c.API().GetProductionMapChunk(ctx, &apiclient.GetProductionMapChunkRequestOptions{
		PathParams: &apiclient.GetProductionMapChunkPath{SetID: parsedSet, Revision: revision, MemberID: parsedMember},
		Query:      query})
	if err != nil {
		return api.ProductionMapChunk{}, err
	}
	if result == nil || !canonical.IsSHA256Hex(result.MapSHA256) || !canonical.IsSHA256Hex(result.ChunkSHA256) ||
		result.Offset < 0 || result.TotalBytes < 1 || result.Offset >= result.TotalBytes || len(result.NextCursor) > 512 {
		return api.ProductionMapChunk{}, integrityErrorf("production map page is inconsistent")
	}
	data, err := base64.StdEncoding.Strict().DecodeString(result.Data)
	if err != nil || len(data) < 1 || len(data) > limit || result.Offset+int64(len(data)) > result.TotalBytes ||
		(result.NextCursor == "") != (result.Offset+int64(len(data)) == result.TotalBytes) ||
		result.NextCursor == cursor {
		return api.ProductionMapChunk{}, integrityErrorf("production map page data is inconsistent")
	}
	digest := sha256.Sum256(data)
	if hex.EncodeToString(digest[:]) != result.ChunkSHA256 {
		return api.ProductionMapChunk{}, integrityErrorf("production map page digest is inconsistent")
	}
	return *result, nil
}

func productionSetUUID(value string) (uuid.UUID, error) {
	if !validUUIDv4(value) {
		return uuid.UUID{}, errors.New("invalid production set ID")
	}
	parsed, err := uuid.Parse(value)
	if err != nil {
		return uuid.UUID{}, fmt.Errorf("parsing production set ID: %w", err)
	}
	return parsed, nil
}

func (c *Connection) EditProductionInstructions(ctx context.Context, setID string, revision, etag int64,
	request api.ProductionInstructionsRequest) (redaction.Receipt, error) {
	parsed, err := productionSetUUID(setID)
	if err != nil || revision < 1 || redaction.ValidateInstructionsEditRequest(request.Domain(etag)) != nil {
		return redaction.Receipt{}, errors.New("invalid production instructions edit")
	}
	header := strconv.FormatInt(etag, 10)
	result, err := c.API().EditProductionInstructions(ctx, &apiclient.EditProductionInstructionsRequestOptions{
		PathParams: &apiclient.EditProductionInstructionsPath{SetID: parsed, Revision: revision},
		Header:     &apiclient.EditProductionInstructionsHeaders{IfMatch: &header}, Body: &request})
	if err != nil {
		return redaction.Receipt{}, err
	}
	return checkedProductionMutationReceipt(result, setID, revision, request.OperationID, etag)
}

func (c *Connection) ApplyProductionChanges(ctx context.Context, setID string, revision, etag int64,
	request api.ProductionChangesRequest) (redaction.Receipt, error) {
	parsed, err := productionSetUUID(setID)
	if err != nil || revision < 1 || redaction.ValidateApplyRequest(request.Domain(etag)) != nil {
		return redaction.Receipt{}, errors.New("invalid production change batch")
	}
	header := strconv.FormatInt(etag, 10)
	result, err := c.API().ApplyProductionChanges(ctx, &apiclient.ApplyProductionChangesRequestOptions{
		PathParams: &apiclient.ApplyProductionChangesPath{SetID: parsed, Revision: revision},
		Header:     &apiclient.ApplyProductionChangesHeaders{IfMatch: &header}, Body: &request})
	if err != nil {
		return redaction.Receipt{}, err
	}
	return checkedProductionMutationReceipt(result, setID, revision, request.OperationID, etag)
}

func checkedProductionMutationReceipt(value *api.ProductionReceipt, setID string, revision int64,
	operationID string, etag int64) (redaction.Receipt, error) {
	if value == nil {
		return redaction.Receipt{}, integrityErrorf("production mutation receipt is missing")
	}
	receipt := redaction.Receipt(*value)
	if redaction.ValidateReceipt(receipt) != nil || receipt.SetID != setID ||
		receipt.Revision != revision || receipt.OperationID != operationID || receipt.ETag <= etag {
		return redaction.Receipt{}, integrityErrorf("production mutation receipt is inconsistent")
	}
	return receipt, nil
}

func (c *Connection) SealProductionMembership(ctx context.Context, setID string, revision, etag int64,
	request api.ProductionMembershipSealRequest) (redaction.Receipt, error) {
	parsed, err := productionSetUUID(setID)
	if err != nil || revision < 1 || redaction.ValidateMembershipSealRequest(request.Domain(etag)) != nil {
		return redaction.Receipt{}, errors.New("invalid production membership seal")
	}
	header := strconv.FormatInt(etag, 10)
	result, err := c.API().SealProductionMembership(ctx, &apiclient.SealProductionMembershipRequestOptions{
		PathParams: &apiclient.SealProductionMembershipPath{SetID: parsed, Revision: revision},
		Header:     &apiclient.SealProductionMembershipHeaders{IfMatch: &header}, Body: &request})
	if err != nil {
		return redaction.Receipt{}, err
	}
	return checkedProductionMutationReceipt(result, setID, revision, request.OperationID, etag)
}

func (c *Connection) ReviewProductionMember(ctx context.Context, setID string, revision, etag int64,
	memberID string, request api.ProductionMemberReviewRequest) (redaction.Receipt, error) {
	parsed, err := productionSetUUID(setID)
	if err != nil || revision < 1 || etag < 1 || !validUUIDv4(memberID) ||
		!validUUIDv4(request.OperationID) || !validSHA256Hex(request.Binding) || !request.Complete {
		return redaction.Receipt{}, errors.New("invalid production member review")
	}
	header := strconv.FormatInt(etag, 10)
	result, err := c.API().ReviewProductionMember(ctx, &apiclient.ReviewProductionMemberRequestOptions{
		PathParams: &apiclient.ReviewProductionMemberPath{SetID: parsed, Revision: revision,
			MemberID: uuid.MustParse(memberID)},
		Header: &apiclient.ReviewProductionMemberHeaders{IfMatch: &header}, Body: &request})
	if err != nil {
		return redaction.Receipt{}, err
	}
	return checkedProductionMutationReceipt(result, setID, revision, request.OperationID, etag)
}
