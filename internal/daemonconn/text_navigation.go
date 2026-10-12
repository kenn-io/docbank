package daemonconn

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http"
	"unicode/utf8"

	"github.com/doordash-oss/oapi-codegen-dd/v3/pkg/runtime"
	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/internal/apiclient"
)

// GetTextOutline checks bounds and pagination of an exact retained-text outline.
func (c *Connection) GetTextOutline(
	ctx context.Context, request document.TextOutlineRequest,
) (document.TextOutlinePage, error) {
	if err := document.ValidateTextOutlineRequest(request); err != nil {
		return document.TextOutlinePage{}, err
	}
	var response *http.Response
	_, err := c.apiWithResponse(&response).GetTextOutline(runtime.WithStreamingResponse(ctx),
		&apiclient.GetTextOutlineRequestOptions{Body: &request})
	if err != nil {
		return document.TextOutlinePage{}, err
	}
	defer func() { _ = response.Body.Close() }()
	var transport struct {
		document.TextOutlinePage
		Schema string `json:"$schema,omitzero"`
	}
	if err := decodeCitationResponse(response, &transport, 256<<10); err != nil {
		return document.TextOutlinePage{}, err
	}
	if err := validateTextOutlinePage(request, transport.TextOutlinePage); err != nil {
		return document.TextOutlinePage{}, err
	}
	return transport.TextOutlinePage, nil
}

// ReadTextSection checks the exact page range and its independently verified quote bytes.
func (c *Connection) ReadTextSection(
	ctx context.Context, request document.TextSectionRequest,
) (document.TextSectionPage, error) {
	if err := document.ValidateTextSectionRequest(request); err != nil {
		return document.TextSectionPage{}, err
	}
	var response *http.Response
	_, err := c.apiWithResponse(&response).ReadTextSection(runtime.WithStreamingResponse(ctx),
		&apiclient.ReadTextSectionRequestOptions{Body: &request})
	if err != nil {
		return document.TextSectionPage{}, err
	}
	defer func() { _ = response.Body.Close() }()
	var transport struct {
		document.TextSectionPage
		Schema string `json:"$schema,omitzero"`
	}
	if err := decodeCitationResponse(response, &transport, 256<<10); err != nil {
		return document.TextSectionPage{}, err
	}
	if err := validateTextSectionPage(request, transport.TextSectionPage); err != nil {
		return document.TextSectionPage{}, err
	}
	return transport.TextSectionPage, nil
}

func validateTextOutlinePage(
	request document.TextOutlineRequest, page document.TextOutlinePage,
) error {
	invalid := errors.New("text outline response does not match its request or bounds")
	if page.Citation != request.Citation || page.Offset != request.Offset ||
		page.DocumentChars < request.Citation.End || page.DocumentChars > 2147483647 ||
		page.Total < 1 || page.Total > document.MaxTextNavigationHeadings+1 || page.Offset > page.Total ||
		len(page.Sections) != min(document.TextOutlinePageSize, page.Total-page.Offset) ||
		page.EnclosingSection.Start < 0 || page.EnclosingSection.Start > request.Citation.Start ||
		page.EnclosingSection.End < request.Citation.End || page.EnclosingSection.End > page.DocumentChars {
		return invalid
	}
	end := page.Offset + len(page.Sections)
	if end < page.Total {
		if page.NextOffset == nil || *page.NextOffset != end {
			return invalid
		}
	} else if page.NextOffset != nil {
		return invalid
	}
	for i, section := range page.Sections {
		titleChars := utf8.RuneCountInString(section.TitleMarkdown)
		if section.Start < 0 || section.End <= section.Start || section.End > page.DocumentChars ||
			!utf8.ValidString(section.TitleMarkdown) || titleChars > document.MaxTextOutlineTitleChars ||
			(section.TitleTruncated && titleChars != document.MaxTextOutlineTitleChars) {
			return invalid
		}
		if page.Offset+i == 0 {
			if section.Start != 0 || section.End != page.DocumentChars || section.Level != 0 ||
				section.TitleMarkdown != "" || section.TitleTruncated {
				return invalid
			}
			continue
		}
		if section.Level < 1 || section.Level > 6 {
			return invalid
		}
		if i > 0 && page.Offset+i > 1 && section.Start <= page.Sections[i-1].Start {
			return invalid
		}
	}
	return nil
}

func validateTextSectionPage(
	request document.TextSectionRequest, page document.TextSectionPage,
) error {
	start := request.Section.Start + request.Offset
	end := start + min(request.MaxChars, request.Section.End-start)
	digest := sha256.Sum256([]byte(page.Text))
	if page.Citation != request.Citation || page.Section != request.Section || page.Offset != request.Offset ||
		page.ActualStart != start || page.ActualEnd != end || page.NextOffset != end-request.Section.Start ||
		page.Complete != (end == request.Section.End) || !utf8.ValidString(page.Text) ||
		utf8.RuneCountInString(page.Text) != end-start || page.TextBytes != len(page.Text) ||
		page.TextSHA256 != hex.EncodeToString(digest[:]) {
		return errors.New("text section response does not match its request or quotation")
	}
	return nil
}
