package daemonconn

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json/v2"
	"errors"
	"io"
	"net/http"
	"unicode/utf8"

	"github.com/doordash-oss/oapi-codegen-dd/v3/pkg/runtime"

	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/apiclient"
)

const (
	maxPassageOutlineResponseBytes = 384 << 10
	maxPassageResolveEnvelopeBytes = 16 << 10
	defaultPassageSectionBytes     = 32 << 10
	maxPassageSectionBytes         = 256 << 10
	maxPassageSectionEnvelopeBytes = 16 << 10
	maxPassageOutlineSections      = 4096
)

// ResolvePassage reads one supplied exact retained passage. The response is
// bounded and must bind the requested reference and its exact quote bytes.
func (c *Connection) ResolvePassage(
	ctx context.Context, request api.PassageResolveRequest,
) (api.PassageResolution, error) {
	limit := request.MaxBytes
	if limit == 0 {
		limit = defaultPassageSectionBytes
	}
	if err := document.ValidatePassageAddressV1(request.Ref); err != nil ||
		limit < 1 || limit > maxPassageSectionBytes {
		return api.PassageResolution{}, errors.New("passage resolve request is invalid")
	}
	var responseHTTP *http.Response
	_, err := c.apiWithResponse(&responseHTTP).ResolvePassage(runtime.WithStreamingResponse(ctx),
		&apiclient.ResolvePassageRequestOptions{Body: &request})
	if err != nil {
		return api.PassageResolution{}, err
	}
	result, err := decodeBoundedPassageResponse[api.PassageResolution](responseHTTP,
		6*limit+maxPassageResolveEnvelopeBytes, "passage resolve")
	if err != nil {
		return api.PassageResolution{}, err
	}
	id, err := document.PassageIdentityV1(request.Ref)
	if err != nil || result.Ref != request.Ref || result.Availability != "available" ||
		(result.Freshness != "current" && result.Freshness != "historical") ||
		result.PassageID != id || len(result.Text) != request.Ref.ByteEnd-request.Ref.ByteStart ||
		len(result.Text) > limit || !utf8.ValidString(result.Text) {
		return api.PassageResolution{}, errors.New("passage resolve response does not bind its requested authority")
	}
	quote := sha256.Sum256([]byte(result.Text))
	if hex.EncodeToString(quote[:]) != request.Ref.QuoteSHA256 {
		return api.PassageResolution{}, errors.New("passage resolve response quote is invalid")
	}
	return result, nil
}

// PassageOutline reads the bounded structural outline for one exact retained
// rendition. The response is rejected unless every section remains inside its
// parent and all advertised identity fields bind the request.
func (c *Connection) PassageOutline(
	ctx context.Context, request api.PassageOutlineRequest,
) (api.PassageOutline, error) {
	if err := document.ValidatePassageAddressV1(request.Ref); err != nil {
		return api.PassageOutline{}, errors.New("passage outline request is invalid")
	}
	var responseHTTP *http.Response
	_, err := c.apiWithResponse(&responseHTTP).OutlineDocument(runtime.WithStreamingResponse(ctx),
		&apiclient.OutlineDocumentRequestOptions{Body: &request})
	if err != nil {
		return api.PassageOutline{}, err
	}
	result, err := decodeBoundedPassageResponse[api.PassageOutline](responseHTTP,
		maxPassageOutlineResponseBytes, "passage outline")
	if err != nil {
		return api.PassageOutline{}, err
	}
	if err := validatePassageOutline(request, result); err != nil {
		return api.PassageOutline{}, err
	}
	return result, nil
}

// ReadPassageSection reads one bounded page from an exact retained section.
// It validates the response's authority and page coordinates before returning
// source text to the caller.
func (c *Connection) ReadPassageSection(
	ctx context.Context, request api.PassageReadSectionRequest,
) (api.PassageSectionPage, error) {
	limit := request.MaxBytes
	if limit == 0 {
		limit = defaultPassageSectionBytes
	}
	if err := document.ValidatePassageAddressV1(request.Ref); err != nil ||
		!validSHA256Hex(request.NavigationKey) || limit < 1 || limit > maxPassageSectionBytes ||
		len(request.Continuation) > 4096 {
		return api.PassageSectionPage{}, errors.New("passage section request is invalid")
	}
	var responseHTTP *http.Response
	_, err := c.apiWithResponse(&responseHTTP).ReadPassageSection(runtime.WithStreamingResponse(ctx),
		&apiclient.ReadPassageSectionRequestOptions{Body: &request})
	if err != nil {
		return api.PassageSectionPage{}, err
	}
	// MaxBytes bounds the decoded text, not its JSON envelope. Escaping can
	// expand one UTF-8 byte to six JSON bytes (for example, a control byte).
	result, err := decodeBoundedPassageResponse[api.PassageSectionPage](responseHTTP,
		6*limit+maxPassageSectionEnvelopeBytes,
		"passage section")
	if err != nil {
		return api.PassageSectionPage{}, err
	}
	if len(result.Text) > limit {
		return api.PassageSectionPage{}, errors.New("passage section text exceeds requested limit")
	}
	if err := validatePassageSectionPage(request, result); err != nil {
		return api.PassageSectionPage{}, err
	}
	return result, nil
}

func decodeBoundedPassageResponse[T any](response *http.Response, limit int, kind string) (T, error) {
	var zero T
	if response == nil || response.Body == nil {
		return zero, errors.New(kind + " response is unavailable")
	}
	defer func() { _ = response.Body.Close() }()
	encoded, err := io.ReadAll(io.LimitReader(response.Body, int64(limit)+1))
	if err != nil {
		return zero, errors.New("reading " + kind + " response")
	}
	if len(encoded) > limit {
		return zero, errors.New(kind + " response is too large")
	}
	var result T
	if err := json.Unmarshal(encoded, &result, json.RejectUnknownMembers(true)); err != nil {
		return zero, errors.New(kind + " response is invalid")
	}
	return result, nil
}

func validatePassageOutline(request api.PassageOutlineRequest, outline api.PassageOutline) error {
	if outline.BodySHA256 != request.Ref.BodySHA256 ||
		outline.RenditionBuildID != request.Ref.RenditionBuildID || len(outline.Sections) == 0 {
		return errors.New("passage outline response does not bind its requested authority")
	}
	seen := make(map[string]struct{}, len(outline.Sections))
	count := 0
	previousEnd := 0
	for index, section := range outline.Sections {
		if index == 0 {
			if !section.Preamble || section.Level != 0 || section.Title != "" ||
				section.ByteStart != 0 || section.Occurrence != 1 {
				return errors.New("passage outline response has an invalid preamble")
			}
		} else if section.Preamble || section.Level < 1 || section.ByteStart < previousEnd {
			return errors.New("passage outline response has invalid root sections")
		}
		if err := validatePassageOutlineSection(section, nil, seen, &count); err != nil {
			return err
		}
		previousEnd = section.ByteEnd
	}
	return nil
}

func validatePassageOutlineSection(section api.PassageOutlineSection, parent *api.PassageOutlineSection,
	seen map[string]struct{}, count *int,
) error {
	*count++
	if *count > maxPassageOutlineSections || !validSHA256Hex(section.Key) ||
		utf8.RuneCountInString(section.Title) > 8192 ||
		section.Occurrence < 1 || section.Level < 0 || section.Level > 6 ||
		section.ByteStart < 0 || section.OwnByteEnd < section.ByteStart ||
		section.ByteEnd < section.OwnByteEnd ||
		section.EstimatedUTF8Bytes != section.ByteEnd-section.ByteStart ||
		section.EstimatedRunes < 0 || section.EstimatedRunes > section.EstimatedUTF8Bytes ||
		section.ChildCount != len(section.Children) {
		return errors.New("passage outline response contains an invalid section")
	}
	if _, duplicate := seen[section.Key]; duplicate {
		return errors.New("passage outline response repeats a section key")
	}
	seen[section.Key] = struct{}{}
	if parent != nil && (section.Preamble || section.Level <= parent.Level ||
		section.ByteStart < parent.ByteStart || section.ByteEnd > parent.ByteEnd) {
		return errors.New("passage outline response contains an invalid hierarchy")
	}
	previousEnd := section.OwnByteEnd
	for index := range section.Children {
		child := section.Children[index]
		if child.ByteStart < previousEnd {
			return errors.New("passage outline response contains overlapping children")
		}
		if err := validatePassageOutlineSection(child, &section, seen, count); err != nil {
			return err
		}
		previousEnd = child.ByteEnd
	}
	return nil
}

func validatePassageSectionPage(request api.PassageReadSectionRequest, page api.PassageSectionPage) error {
	section := page.Section
	if page.BodySHA256 != request.Ref.BodySHA256 ||
		page.RenditionBuildID != request.Ref.RenditionBuildID ||
		section.Key != request.NavigationKey || section.IncludeChildren != request.IncludeChildren ||
		section.ByteStart < 0 || section.ByteEnd < section.ByteStart ||
		page.PageStart < section.ByteStart || page.PageEnd < page.PageStart || page.PageEnd > section.ByteEnd ||
		!utf8.ValidString(page.Text) || page.PageEnd-page.PageStart != len(page.Text) {
		return errors.New("passage section response does not bind its requested authority")
	}
	if page.Complete {
		if page.PageEnd != section.ByteEnd || page.Continuation != "" {
			return errors.New("passage section response has invalid completion state")
		}
	} else if page.PageEnd >= section.ByteEnd || page.Continuation == "" ||
		len(page.Continuation) > 4096 {
		return errors.New("passage section response has invalid continuation state")
	}
	if page.Text == "" {
		if page.Ref != nil || !page.Complete || page.PageStart != page.PageEnd {
			return errors.New("passage section response has an invalid empty page")
		}
		return nil
	}
	if page.Ref == nil || !passagePageRefMatches(request.Ref, *page.Ref, page.PageStart, page.PageEnd,
		page.Text) {
		return errors.New("passage section response has an invalid page reference")
	}
	return nil
}

func passagePageRefMatches(base, page document.PassageRefV1, start, end int, text string) bool {
	if document.ValidatePassageAddressV1(page) != nil || page.Version != base.Version ||
		page.FederationDomainUID != base.FederationDomainUID || page.VaultUID != base.VaultUID ||
		page.DocumentUID != base.DocumentUID || page.ContentVersionID != base.ContentVersionID ||
		page.SourceSHA256 != base.SourceSHA256 || page.RenditionBuildID != base.RenditionBuildID ||
		page.AttachmentID != base.AttachmentID || page.BodySHA256 != base.BodySHA256 ||
		page.ByteStart != start || page.ByteEnd != end {
		return false
	}
	quote := sha256.Sum256([]byte(text))
	return page.QuoteSHA256 == hex.EncodeToString(quote[:])
}
