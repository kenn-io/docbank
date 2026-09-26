package processing

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"sync"
	"unicode/utf8"

	"github.com/yuin/goldmark"
	"github.com/yuin/goldmark/ast"
	"github.com/yuin/goldmark/text"

	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/internal/store"
)

const (
	sectionContinuationDomain = "docbank-section-continuation/v1\x00"
	// Outlining parses the whole body, so it has a smaller artifact ceiling
	// than streaming quote resolution: 64 MiB of body plus 256 KiB of envelope.
	maxOutlineArtifactBytes   = int64((64 << 20) + (256 << 10))
	maxSectionContinuation    = 4096
	maxOutlineSections        = 4096
	maxOutlineTitleCharacters = 8192
	maxOutlineWireBytes       = 320 << 10
)

var (
	ErrSectionNotFound     = errors.New("section navigation key is unavailable")
	ErrSectionBudget       = errors.New("section response budget is too small")
	ErrSectionContinuation = errors.New("section continuation is invalid")
	ErrOutlineTooLarge     = errors.New("outline exceeds the response limit")
	sectionContinuationKey = sync.OnceValues(func() ([32]byte, error) {
		var key [32]byte
		_, err := io.ReadFull(rand.Reader, key[:])
		return key, err
	})
)

// PassageOutline describes structural sections within one exact retained
// Markdown body. Sections always begins with an explicit preamble.
type PassageOutline struct {
	BodySHA256       string           `json:"body_sha256"`
	RenditionBuildID string           `json:"rendition_build_id"`
	Sections         []OutlineSection `json:"sections"`
}

// OutlineRequest selects one exact authorized retained rendition.
type OutlineRequest struct {
	Ref       document.PassageRefV1 `json:"ref"`
	principal string
}

// OutlineSection is one stable, half-open body range. ByteEnd includes all
// descendants; OwnByteEnd stops before the first child heading.
type OutlineSection struct {
	Key                string                      `json:"key"`
	Title              string                      `json:"title"`
	Level              int                         `json:"level"`
	Occurrence         int                         `json:"occurrence"`
	ByteStart          int                         `json:"byte_start"`
	ByteEnd            int                         `json:"byte_end"`
	OwnByteEnd         int                         `json:"own_byte_end"`
	EstimatedUTF8Bytes int                         `json:"estimated_utf8_bytes"`
	EstimatedRunes     int                         `json:"estimated_runes"`
	ChildCount         int                         `json:"child_count"`
	Preamble           bool                        `json:"preamble"`
	SourceLocator      *document.EvidenceLocatorV1 `json:"source_locator,omitempty"`
	Children           []OutlineSection            `json:"children"`
}

// SectionReadRequest selects one exact outline section and an optional signed
// continuation. MaxBytes applies to the complete serialized response.
type SectionReadRequest struct {
	Ref             document.PassageRefV1 `json:"ref"`
	NavigationKey   string                `json:"navigation_key"`
	IncludeChildren bool                  `json:"include_children"`
	MaxBytes        int                   `json:"max_bytes,omitzero"`
	Continuation    string                `json:"continuation,omitzero"`
}

// SectionSelection binds a page to the exact section range selected by the
// caller without repeating the outline's child tree on every page.
type SectionSelection struct {
	Key             string                      `json:"key"`
	Title           string                      `json:"title"`
	Level           int                         `json:"level"`
	ByteStart       int                         `json:"byte_start"`
	ByteEnd         int                         `json:"byte_end"`
	IncludeChildren bool                        `json:"include_children"`
	SourceLocator   *document.EvidenceLocatorV1 `json:"source_locator,omitempty"`
}

// SectionReadResult is one bounded page. Complete is false whenever a signed
// continuation is required to recover the rest of the section.
type SectionReadResult struct {
	BodySHA256       string                 `json:"body_sha256"`
	RenditionBuildID string                 `json:"rendition_build_id"`
	Section          SectionSelection       `json:"section"`
	Text             string                 `json:"text"`
	Ref              *document.PassageRefV1 `json:"ref,omitempty"`
	PageStart        int                    `json:"page_start"`
	PageEnd          int                    `json:"page_end"`
	Complete         bool                   `json:"complete"`
	Continuation     string                 `json:"continuation,omitzero"`
}

type sectionContinuationClaim struct {
	Version          string `json:"version"`
	Principal        string `json:"principal"`
	Scope            string `json:"scope"`
	VaultUID         string `json:"vault_uid"`
	DocumentUID      string `json:"document_uid"`
	ContentVersionID string `json:"content_version_id"`
	AttachmentID     string `json:"attachment_id"`
	RenditionBuildID string `json:"rendition_build_id"`
	BodySHA256       string `json:"body_sha256"`
	SectionKey       string `json:"section_key"`
	SectionStart     int    `json:"section_start"`
	SectionEnd       int    `json:"section_end"`
	NextByte         int    `json:"next_byte"`
	IncludeChildren  bool   `json:"include_children"`
}

type outlineHeading struct {
	title string
	level int
	start int
}

// PassageOutline returns the hierarchy for the exact retained rendition named
// by request. It never substitutes a current rendition or invokes a provider.
func (service *Service) PassageOutline(ctx context.Context, request OutlineRequest) (PassageOutline, error) {
	if service == nil || service.catalog == nil || service.blobs == nil {
		return PassageOutline{}, ErrPassageUnavailable
	}
	request.principal = service.principal
	return outlinePassage(ctx, service.catalog, service.blobs, request)
}

// ReadPassageSection returns one bounded page from an exact outline section.
func (service *Service) ReadPassageSection(
	ctx context.Context, request SectionReadRequest,
) (SectionReadResult, error) {
	if service == nil || service.catalog == nil || service.blobs == nil {
		return SectionReadResult{}, ErrPassageUnavailable
	}
	key, err := sectionContinuationKey()
	if err != nil {
		return SectionReadResult{}, ErrPassageUnavailable
	}
	return readAuthorizedPassageSection(ctx, service.catalog, service.blobs, request,
		key, service.principal, service.scope)
}

func outlinePassage(
	ctx context.Context, catalog passageAuthorityCatalog, blobs verifiedBlobReader, request OutlineRequest,
) (PassageOutline, error) {
	frontmatter, body, authority, err := loadPassageRendition(ctx, catalog, blobs, request.Ref, request.principal)
	if err != nil {
		return PassageOutline{}, err
	}
	return buildPassageOutline(body, frontmatter.Rendition.BuildID, frontmatter.Rendition.BodySHA256,
		frontmatter.Navigation.Entries, authority.Build.Units)
}

func readAuthorizedPassageSection(
	ctx context.Context, catalog passageAuthorityCatalog, blobs verifiedBlobReader,
	request SectionReadRequest, key [32]byte, principal, scope string,
) (SectionReadResult, error) {
	frontmatter, body, authority, err := loadPassageRendition(ctx, catalog, blobs, request.Ref, principal)
	if err != nil {
		return SectionReadResult{}, err
	}
	outline, err := buildPassageOutline(body, frontmatter.Rendition.BuildID, frontmatter.Rendition.BodySHA256,
		frontmatter.Navigation.Entries, authority.Build.Units)
	if err != nil {
		return SectionReadResult{}, err
	}
	return readPassageSection(body, outline, request, key, principal, scope)
}

func loadPassageRendition(
	ctx context.Context, catalog passageAuthorityCatalog, blobs verifiedBlobReader,
	ref document.PassageRefV1, principal string,
) (document.RenditionFrontMatterV1, []byte, store.PassageAuthority, error) {
	if catalog == nil || blobs == nil {
		return document.RenditionFrontMatterV1{}, nil, store.PassageAuthority{}, ErrPassageUnavailable
	}
	if err := document.ValidatePassageAddressV1(ref); err != nil {
		return document.RenditionFrontMatterV1{}, nil, store.PassageAuthority{}, ErrPassageInvalid
	}
	if ref.FederationDomainUID == "" && ref.VaultUID != catalog.VaultID() {
		return document.RenditionFrontMatterV1{}, nil, store.PassageAuthority{}, ErrPassageUnauthorized
	}
	authority, err := catalog.ResolvePassageAuthority(ctx, ref)
	if errors.Is(err, store.ErrDocumentIdentityUnavailable) ||
		errors.Is(err, store.ErrPassageAuthorityUnavailable) || errors.Is(err, store.ErrNotFound) {
		return document.RenditionFrontMatterV1{}, nil, store.PassageAuthority{}, ErrPassageUnavailable
	}
	if err != nil {
		return document.RenditionFrontMatterV1{}, nil, store.PassageAuthority{}, err
	}
	if err := checkPassageInputVisibility(ctx, catalog, authority, principal); err != nil {
		return document.RenditionFrontMatterV1{}, nil, store.PassageAuthority{}, err
	}
	if authority.Artifact.Size < 1 || authority.Artifact.Size > maxOutlineArtifactBytes {
		return document.RenditionFrontMatterV1{}, nil, store.PassageAuthority{}, ErrPassageUnavailable
	}
	stream, size, err := blobs.OpenStreamContext(ctx, authority.Artifact.BlobHash)
	if err != nil {
		return document.RenditionFrontMatterV1{}, nil, store.PassageAuthority{}, ErrPassageUnavailable
	}
	defer func() { _ = stream.Close() }()
	if size != authority.Artifact.Size {
		return document.RenditionFrontMatterV1{}, nil, store.PassageAuthority{}, ErrPassageCorrupt
	}
	// Parsing needs the complete Markdown body, bounded by the outline cap.
	payload, err := io.ReadAll(io.LimitReader(stream, size+1))
	if err != nil || int64(len(payload)) != size {
		return document.RenditionFrontMatterV1{}, nil, store.PassageAuthority{}, ErrPassageCorrupt
	}
	if err := stream.Verify(); err != nil || !stream.Verified() {
		return document.RenditionFrontMatterV1{}, nil, store.PassageAuthority{}, ErrPassageCorrupt
	}
	digest := sha256.Sum256(payload)
	if hex.EncodeToString(digest[:]) != authority.Artifact.BlobHash {
		return document.RenditionFrontMatterV1{}, nil, store.PassageAuthority{}, ErrPassageCorrupt
	}
	frontmatter, body, err := document.ParseRenditionFrontMatterV1(payload)
	if err != nil || frontmatter.Source.SHA256 != ref.SourceSHA256 ||
		frontmatter.Rendition.BuildID != authority.Build.ID ||
		frontmatter.Rendition.BodySHA256 != ref.BodySHA256 {
		return document.RenditionFrontMatterV1{}, nil, store.PassageAuthority{}, ErrPassageCorrupt
	}
	if err := document.ValidatePassageRefV1(ref, body); err != nil {
		return document.RenditionFrontMatterV1{}, nil, store.PassageAuthority{}, fmt.Errorf("%w: %w", ErrPassageCorrupt, err)
	}
	return frontmatter, body, authority, nil
}

func buildPassageOutline(
	body []byte, buildID, bodySHA256 string,
	navigation []document.RenditionNavigationEntryV1, units []store.RenditionUnitRecord,
) (PassageOutline, error) {
	if !utf8.Valid(body) {
		return PassageOutline{}, ErrPassageInvalid
	}
	headings, err := passageHeadings(body)
	if err != nil {
		return PassageOutline{}, err
	}
	if len(headings) > maxOutlineSections-1 {
		return PassageOutline{}, ErrOutlineTooLarge
	}
	firstHeading := len(body)
	if len(headings) != 0 {
		firstHeading = headings[0].start
	}
	preamble := outlineSection(body, buildID, bodySHA256, "", 0, 1, 0, firstHeading, firstHeading, true,
		navigation, units)
	flat := make([]OutlineSection, len(headings))
	occurrences := make(map[string]int, len(headings))
	for index, heading := range headings {
		occurrences[heading.title]++
		end := len(body)
		for next := index + 1; next < len(headings); next++ {
			if headings[next].level <= heading.level {
				end = headings[next].start
				break
			}
		}
		ownEnd := end
		if index+1 < len(headings) && headings[index+1].start < end {
			ownEnd = headings[index+1].start
		}
		flat[index] = outlineSection(body, buildID, bodySHA256, heading.title, heading.level,
			occurrences[heading.title], heading.start, end, ownEnd, false, navigation, units)
	}
	roots, _ := nestOutlineSections(flat, 0, 0)
	sections := make([]OutlineSection, 0, len(roots)+1)
	sections = append(sections, preamble)
	sections = append(sections, roots...)
	outline := PassageOutline{BodySHA256: bodySHA256, RenditionBuildID: buildID, Sections: sections}
	encoded, err := json.Marshal(outline)
	if err != nil {
		return PassageOutline{}, fmt.Errorf("marshal passage outline: %w", err)
	}
	if len(encoded) > maxOutlineWireBytes {
		return PassageOutline{}, ErrOutlineTooLarge
	}
	return outline, nil
}

func passageHeadings(body []byte) ([]outlineHeading, error) {
	doc := goldmark.DefaultParser().Parse(text.NewReader(body))
	result := make([]outlineHeading, 0, 32)
	err := ast.Walk(doc, func(node ast.Node, entering bool) (ast.WalkStatus, error) {
		if !entering {
			return ast.WalkContinue, nil
		}
		heading, ok := node.(*ast.Heading)
		if !ok {
			return ast.WalkContinue, nil
		}
		if heading.Lines().Len() == 0 {
			return ast.WalkContinue, nil
		}
		contentStart := heading.Lines().At(0).Start
		start := contentStart
		for start > 0 && body[start-1] != '\n' {
			start--
		}
		title := truncateUTF8Characters(strings.TrimSpace(passageHeadingText(heading, body)),
			maxOutlineTitleCharacters)
		result = append(result, outlineHeading{title: title,
			level: heading.Level, start: start})
		return ast.WalkSkipChildren, nil
	})
	if err != nil {
		return nil, fmt.Errorf("walk Markdown outline: %w", err)
	}
	return result, nil
}

func truncateUTF8Characters(value string, maximum int) string {
	if maximum < 1 {
		return ""
	}
	count := 0
	for index := range value {
		if count == maximum {
			return value[:index]
		}
		count++
	}
	return value
}

func passageHeadingText(heading *ast.Heading, source []byte) string {
	var result strings.Builder
	var appendNode func(ast.Node)
	appendNode = func(node ast.Node) {
		switch typed := node.(type) {
		case *ast.Text:
			result.Write(typed.Value(source))
			if typed.SoftLineBreak() {
				result.WriteByte('\n')
			}
		case *ast.String:
			result.Write(typed.Value)
		case *ast.RawHTML:
			result.Write(typed.Segments.Value(source))
		}
		for child := node.FirstChild(); child != nil; child = child.NextSibling() {
			appendNode(child)
		}
	}
	for child := heading.FirstChild(); child != nil; child = child.NextSibling() {
		appendNode(child)
	}
	return result.String()
}

func outlineSection(
	body []byte, buildID, bodySHA256, title string, level, occurrence, start, end, ownEnd int,
	preamble bool, navigation []document.RenditionNavigationEntryV1, units []store.RenditionUnitRecord,
) OutlineSection {
	section := OutlineSection{Title: title, Level: level, Occurrence: occurrence,
		ByteStart: start, ByteEnd: end, OwnByteEnd: ownEnd,
		EstimatedUTF8Bytes: end - start, EstimatedRunes: utf8.RuneCount(body[start:end]),
		Preamble: preamble, Children: []OutlineSection{}}
	section.Key = passageSectionKey(buildID, bodySHA256, section)
	section.SourceLocator = exactSectionLocator(start, navigation, units)
	return section
}

func passageSectionKey(buildID, bodySHA256 string, section OutlineSection) string {
	hash := sha256.New()
	for _, value := range []string{"docbank-section/v1", buildID, bodySHA256, section.Title,
		strconv.Itoa(section.Level), strconv.Itoa(section.Occurrence),
		strconv.Itoa(section.ByteStart), strconv.Itoa(section.ByteEnd),
		strconv.Itoa(section.OwnByteEnd), strconv.FormatBool(section.Preamble)} {
		var size [8]byte
		binary.BigEndian.PutUint64(size[:], uint64(len(value)))
		_, _ = hash.Write(size[:])
		_, _ = hash.Write([]byte(value))
	}
	return hex.EncodeToString(hash.Sum(nil))
}

func exactSectionLocator(
	start int, navigation []document.RenditionNavigationEntryV1, units []store.RenditionUnitRecord,
) *document.EvidenceLocatorV1 {
	key := ""
	for _, entry := range navigation {
		if entry.Byte == start {
			key = entry.Key
			break
		}
	}
	if key == "" {
		return nil
	}
	for _, unit := range units {
		if unit.EvidenceUnitID == key {
			locator := unit.Locator
			return &locator
		}
	}
	return nil
}

func nestOutlineSections(flat []OutlineSection, index, parentLevel int) ([]OutlineSection, int) {
	result := make([]OutlineSection, 0)
	for index < len(flat) {
		if flat[index].Level <= parentLevel {
			break
		}
		section := flat[index]
		index++
		section.Children, index = nestOutlineSections(flat, index, section.Level)
		section.ChildCount = len(section.Children)
		result = append(result, section)
	}
	return result, index
}

func readPassageSection(
	body []byte, outline PassageOutline, request SectionReadRequest, key [32]byte, principal, scope string,
) (SectionReadResult, error) {
	if request.MaxBytes == 0 {
		request.MaxBytes = DefaultPassageReadBytes
	}
	if request.MaxBytes < 1 || request.MaxBytes > MaxPassageReadBytes || request.NavigationKey == "" ||
		principal == "" || scope == "" || key == ([32]byte{}) {
		return SectionReadResult{}, ErrPassageInvalid
	}
	if err := document.ValidatePassageRefV1(request.Ref, body); err != nil {
		return SectionReadResult{}, fmt.Errorf("%w: %w", ErrPassageCorrupt, err)
	}
	section, ok := findOutlineSection(outline.Sections, request.NavigationKey)
	if !ok {
		return SectionReadResult{}, ErrSectionNotFound
	}
	end := section.OwnByteEnd
	if request.IncludeChildren {
		end = section.ByteEnd
	}
	offset := section.ByteStart
	if request.Continuation != "" {
		claim, err := verifySectionContinuation(request.Continuation, key)
		if err != nil || !sectionClaimMatches(claim, request, section, end, principal, scope) {
			return SectionReadResult{}, ErrSectionContinuation
		}
		offset = claim.NextByte
	}
	if outline.BodySHA256 != request.Ref.BodySHA256 ||
		offset < section.ByteStart || offset > end {
		return SectionReadResult{}, ErrSectionContinuation
	}
	selection := SectionSelection{Key: section.Key, Title: section.Title, Level: section.Level,
		ByteStart: section.ByteStart, ByteEnd: end, IncludeChildren: request.IncludeChildren}
	if section.SourceLocator != nil {
		locator := *section.SourceLocator
		selection.SourceLocator = &locator
	}
	result, err := fitSectionResponse(body, request, selection, offset, end, key, principal, scope)
	if err != nil {
		return SectionReadResult{}, err
	}
	// An adopted origin reference keeps its original identity in page refs and
	// continuations, while the rendition reported here is the retained local one.
	result.RenditionBuildID = outline.RenditionBuildID
	return result, nil
}

func findOutlineSection(sections []OutlineSection, key string) (OutlineSection, bool) {
	for _, section := range sections {
		if section.Key == key {
			return section, true
		}
		if child, ok := findOutlineSection(section.Children, key); ok {
			return child, true
		}
	}
	return OutlineSection{}, false
}

func fitSectionResponse(
	body []byte, request SectionReadRequest, selection SectionSelection, offset, end int,
	key [32]byte, principal, scope string,
) (SectionReadResult, error) {
	if offset == end {
		result, err := sectionResult(body, request, selection, offset, end, key, principal, scope)
		if err != nil {
			return SectionReadResult{}, err
		}
		encoded, err := json.Marshal(result)
		if err != nil || len(encoded) > request.MaxBytes {
			return SectionReadResult{}, ErrSectionBudget
		}
		return result, nil
	}
	maximum := min(end-offset, request.MaxBytes)
	part, stop, err := document.SectionPage(body, offset, end, maximum)
	if err != nil {
		return SectionReadResult{}, ErrSectionBudget
	}
	_ = part
	if result, fits := fittingSectionResult(body, request, selection, offset, stop, end, key, principal, scope); fits {
		return preferSectionBoundary(body, request, selection, result, offset, end, key, principal, scope), nil
	}
	low, high := 1, maximum-1
	var fitted SectionReadResult
	found := false
	for low <= high {
		middle := low + (high-low)/2
		_, candidateStop, pageErr := document.SectionPage(body, offset, end, middle)
		if pageErr != nil {
			low = middle + 1
			continue
		}
		candidate, fits := fittingSectionResult(body, request, selection, offset, candidateStop, end,
			key, principal, scope)
		if fits {
			fitted, found = candidate, true
			low = middle + 1
		} else {
			high = middle - 1
		}
	}
	if !found {
		return SectionReadResult{}, ErrSectionBudget
	}
	return preferSectionBoundary(body, request, selection, fitted, offset, end, key, principal, scope), nil
}

func fittingSectionResult(
	body []byte, request SectionReadRequest, selection SectionSelection, start, stop, end int,
	key [32]byte, principal, scope string,
) (SectionReadResult, bool) {
	result, err := sectionResult(body, request, selection, start, stop, key, principal, scope)
	if err != nil || stop > end {
		return SectionReadResult{}, false
	}
	encoded, err := json.Marshal(result)
	return result, err == nil && len(encoded) <= request.MaxBytes
}

func preferSectionBoundary(
	body []byte, request SectionReadRequest, selection SectionSelection, fitted SectionReadResult,
	start, end int, key [32]byte, principal, scope string,
) SectionReadResult {
	if fitted.Complete || fitted.PageEnd-start < 1024 {
		return fitted
	}
	window := body[start:fitted.PageEnd]
	minimum := len(window) / 2
	preferred := -1
	if index := bytes.LastIndex(window[minimum:], []byte("\n\n")); index >= 0 {
		preferred = minimum + index + 2
	} else if index := bytes.LastIndex(window[minimum:], []byte("\n")); index >= 0 {
		preferred = minimum + index + 1
	}
	if preferred <= 0 || start+preferred >= end {
		return fitted
	}
	result, fits := fittingSectionResult(body, request, selection, start, start+preferred, end,
		key, principal, scope)
	if !fits {
		return fitted
	}
	return result
}

func sectionResult(
	body []byte, request SectionReadRequest, selection SectionSelection, start, stop int,
	key [32]byte, principal, scope string,
) (SectionReadResult, error) {
	result := SectionReadResult{BodySHA256: request.Ref.BodySHA256,
		RenditionBuildID: request.Ref.RenditionBuildID, Section: selection,
		Text: string(body[start:stop]), PageStart: start, PageEnd: stop, Complete: stop == selection.ByteEnd}
	if start < stop {
		ref, err := document.NewPassageRefV1(request.Ref, body, start, stop)
		if err != nil {
			return SectionReadResult{}, err
		}
		result.Ref = &ref
	}
	if !result.Complete {
		claim := sectionContinuationClaim{Version: "v1", Principal: principal, Scope: scope,
			VaultUID: request.Ref.VaultUID, DocumentUID: request.Ref.DocumentUID,
			ContentVersionID: request.Ref.ContentVersionID, AttachmentID: request.Ref.AttachmentID,
			RenditionBuildID: request.Ref.RenditionBuildID, BodySHA256: request.Ref.BodySHA256,
			SectionKey: selection.Key, SectionStart: selection.ByteStart, SectionEnd: selection.ByteEnd,
			NextByte: stop, IncludeChildren: selection.IncludeChildren}
		var err error
		result.Continuation, err = signSectionContinuation(claim, key)
		if err != nil {
			return SectionReadResult{}, err
		}
	}
	return result, nil
}

func signSectionContinuation(claim sectionContinuationClaim, key [32]byte) (string, error) {
	body, err := json.Marshal(claim, json.Deterministic(true))
	if err != nil {
		return "", err
	}
	mac := hmac.New(sha256.New, key[:])
	_, _ = mac.Write([]byte(sectionContinuationDomain))
	_, _ = mac.Write(body)
	return base64.RawURLEncoding.EncodeToString(body) + "." +
		base64.RawURLEncoding.EncodeToString(mac.Sum(nil)), nil
}

func verifySectionContinuation(token string, key [32]byte) (sectionContinuationClaim, error) {
	var zero sectionContinuationClaim
	if len(token) > maxSectionContinuation {
		return zero, ErrSectionContinuation
	}
	bodyPart, signaturePart, ok := strings.Cut(token, ".")
	if !ok {
		return zero, ErrSectionContinuation
	}
	body, err := base64.RawURLEncoding.DecodeString(bodyPart)
	if err != nil || len(body) > 3072 {
		return zero, ErrSectionContinuation
	}
	signature, err := base64.RawURLEncoding.DecodeString(signaturePart)
	if err != nil {
		return zero, ErrSectionContinuation
	}
	mac := hmac.New(sha256.New, key[:])
	_, _ = mac.Write([]byte(sectionContinuationDomain))
	_, _ = mac.Write(body)
	if !hmac.Equal(signature, mac.Sum(nil)) {
		return zero, ErrSectionContinuation
	}
	var claim sectionContinuationClaim
	if err := json.Unmarshal(body, &claim, json.RejectUnknownMembers(true)); err != nil {
		return zero, ErrSectionContinuation
	}
	return claim, nil
}

func sectionClaimMatches(
	claim sectionContinuationClaim, request SectionReadRequest, section OutlineSection, end int, principal, scope string,
) bool {
	return claim.Version == "v1" && claim.Principal == principal && claim.Scope == scope &&
		claim.VaultUID == request.Ref.VaultUID && claim.DocumentUID == request.Ref.DocumentUID &&
		claim.ContentVersionID == request.Ref.ContentVersionID && claim.AttachmentID == request.Ref.AttachmentID &&
		claim.RenditionBuildID == request.Ref.RenditionBuildID && claim.BodySHA256 == request.Ref.BodySHA256 &&
		claim.SectionKey == section.Key && claim.SectionStart == section.ByteStart && claim.SectionEnd == end &&
		claim.IncludeChildren == request.IncludeChildren && claim.NextByte >= section.ByteStart && claim.NextByte <= end
}
