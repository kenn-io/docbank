package daemonconn

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http"
	"slices"
	"strings"

	"github.com/doordash-oss/oapi-codegen-dd/v3/pkg/runtime"

	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/apiclient"
	"go.kenn.io/docbank/internal/processing"
)

// ContextPack reads a bounded, exact-source context result through the daemon.
// It checks the returned fence and every citation before exposing source text.
func (c *Connection) ContextPack(ctx context.Context, request api.ContextPackRequest) (api.ContextPackResponse, error) {
	limit := request.MaxBytes
	if limit == 0 {
		limit = processing.DefaultContextPackBytes
	}
	if limit < 1 || limit > processing.MaxContextPackBytes ||
		(request.Query == "") == (request.Seed == nil) ||
		len(request.Fence.ContentVersionIDs) < 1 ||
		len(request.Fence.ContentVersionIDs) > processing.MaxSourceFenceIDs {
		return api.ContextPackResponse{}, errors.New("context pack request is invalid")
	}
	fence := processing.SourceFence{VaultUID: request.Fence.VaultUID,
		ContentVersionIDs: request.Fence.ContentVersionIDs}
	fingerprint, err := processing.SourceFenceFingerprint(fence)
	if err != nil {
		return api.ContextPackResponse{}, errors.New("context pack source fence is invalid")
	}
	if request.Seed != nil {
		if document.ValidatePassageAddressV1(*request.Seed) != nil ||
			request.Seed.VaultUID != fence.VaultUID ||
			!containsContextVersion(fence.ContentVersionIDs, request.Seed.ContentVersionID) {
			return api.ContextPackResponse{}, errors.New("context pack seed is outside the source fence")
		}
	}
	var responseHTTP *http.Response
	_, err = c.apiWithResponse(&responseHTTP).CreateContextPack(runtime.WithStreamingResponse(ctx),
		&apiclient.CreateContextPackRequestOptions{Body: &request})
	if err != nil {
		return api.ContextPackResponse{}, err
	}
	result, err := decodeBoundedPassageResponse[api.ContextPackResponse](responseHTTP, limit, "context pack")
	if err != nil {
		return api.ContextPackResponse{}, err
	}
	if err := validateContextPackResponse(request, result, fingerprint); err != nil {
		return api.ContextPackResponse{}, err
	}
	return result, nil
}

func containsContextVersion(ids []string, id string) bool {
	return slices.Contains(ids, id)
}

func validateContextPackResponse(request api.ContextPackRequest, pack api.ContextPackResponse,
	fingerprint string,
) error {
	if pack.FenceFingerprint != fingerprint ||
		pack.Coverage.RequestedSources != len(request.Fence.ContentVersionIDs) ||
		pack.Coverage.AvailableSources < 0 ||
		pack.Coverage.AvailableSources > pack.Coverage.RequestedSources ||
		pack.Coverage.RenditionAvailableSources < 0 ||
		pack.Coverage.RenditionMissingSources < 0 ||
		pack.Coverage.RenditionAvailableSources+pack.Coverage.RenditionMissingSources > pack.Coverage.RequestedSources ||
		pack.Coverage.SelectedSources < 0 ||
		pack.Coverage.SelectedSources > pack.Coverage.AvailableSources ||
		(pack.IndexGenerationID != "" && !validSHA256Hex(pack.IndexGenerationID)) ||
		(pack.IndexManifestDigest != "" && !validSHA256Hex(pack.IndexManifestDigest)) ||
		pack.Complete == pack.Truncated {
		return errors.New("context pack response does not bind its requested source fence")
	}
	selected := map[string]struct{}{}
	for _, passage := range pack.Passages {
		ref := passage.Ref
		if document.ValidatePassageIdentityV1(ref) != nil ||
			ref.VaultUID != request.Fence.VaultUID ||
			!containsContextVersion(request.Fence.ContentVersionIDs, ref.ContentVersionID) ||
			len(passage.Text) != ref.ByteEnd-ref.ByteStart ||
			!strings.HasPrefix(passage.Path, "/") {
			return errors.New("context pack response contains an invalid passage")
		}
		quote := sha256.Sum256([]byte(passage.Text))
		if ref.QuoteSHA256 != hex.EncodeToString(quote[:]) {
			return errors.New("context pack response passage quote does not match its reference")
		}
		selected[ref.DocumentUID] = struct{}{}
	}
	if len(selected) != pack.Coverage.SelectedSources {
		return errors.New("context pack response source coverage is inconsistent")
	}
	for _, count := range pack.Omitted {
		if count < 1 {
			return errors.New("context pack response omission count is invalid")
		}
	}
	if pack.Complete && (len(pack.Omitted) != 0 || pack.SearchTruncated) {
		return errors.New("context pack response completeness is inconsistent")
	}
	return nil
}
