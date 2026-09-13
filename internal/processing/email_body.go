package processing

import (
	"bytes"
	"context"
	"encoding/json/jsontext"
	"errors"
	"io"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/internal/store"
	"golang.org/x/net/html"
)

const emailBodyMaxBytes = 16 << 20
const emailObservationTimeLayout = "2006-01-02T15:04:05.000000000Z"

type emailBodyUnavailableError string

func (e emailBodyUnavailableError) Error() string { return string(e) }

func literalBlock(s string) string {
	longest, run := 0, 0
	for _, r := range s {
		if r == '`' {
			run++
			longest = max(longest, run)
		} else {
			run = 0
		}
	}
	fence := strings.Repeat("`", max(3, longest+1))
	return fence + "\n" + s + "\n" + fence
}
func bodyUnits(ctx context.Context, s string) ([]document.SourceEvidenceUnitV1, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if len(s) > emailBodyMaxBytes {
		return nil, emailBodyUnavailableError("body_text_limit")
	}
	if !utf8.ValidString(s) {
		return nil, errors.New("email body text is not valid UTF-8")
	}
	if strings.ContainsRune(s, '\x00') {
		return nil, emailBodyUnavailableError("unsupported_body")
	}
	var units []document.SourceEvidenceUnitV1
	for len(s) > 0 {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		end := min(len(s), 1<<20)
		for end < len(s) && !utf8.RuneStart(s[end]) {
			end--
		}
		if end == 0 || len(units) >= 17 {
			return nil, errors.New("email body unit boundary invariant failed")
		}
		block := literalBlock(s[:end])
		if len(block) > 4_000_000 {
			return nil, errors.New("email body source unit bound invariant failed")
		}
		units = append(units,
			document.SourceEvidenceUnitV1{Order: len(units),
				Text: block,
				Locator: document.SourceEvidenceLocatorV1{Kind: document.EvidenceLocatorGeneric,
					IndexOrigin: document.EvidenceIndexOriginNone}})
		s = s[end:]
	}
	return units, nil
}

func emailBodyText(ctx context.Context, kind document.EmailBodyKind, body []byte) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if len(body) > emailBodyMaxBytes {
		return "", emailBodyUnavailableError("body_text_limit")
	}
	if !utf8.Valid(body) {
		return "", errors.New("email body artifact is not valid UTF-8")
	}
	switch kind {
	case "plain":
		return string(body), nil
	case "html":
	default:
		return "", emailBodyUnavailableError("unsupported_body")
	}
	tokenizer := html.NewTokenizer(strings.NewReader(string(body)))
	tokenizer.SetMaxBuf(emailBodyMaxBytes + 1)
	var out strings.Builder
	appendText := func(text []byte) error {
		if len(text) > emailBodyMaxBytes-out.Len() {
			return emailBodyUnavailableError("body_text_limit")
		}
		_, _ = out.Write(text)
		return nil
	}
	templateDepth := 0
	rawIgnored := ""
	for {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		token := tokenizer.Next()
		switch token {
		case html.ErrorToken:
			if !errors.Is(tokenizer.Err(), io.EOF) {
				return "", tokenizer.Err()
			}
			return strings.TrimSpace(out.String()), nil
		case html.TextToken:
			if templateDepth == 0 && rawIgnored == "" {
				if err := appendText(tokenizer.Text()); err != nil {
					return "", err
				}
			}
		case html.StartTagToken, html.SelfClosingTagToken, html.EndTagToken:
			name, _ := tokenizer.TagName()
			tag := string(name)
			if rawIgnored != "" {
				if token == html.EndTagToken && tag == rawIgnored {
					rawIgnored = ""
				}
				continue
			}
			if tag == "template" {
				if token == html.EndTagToken {
					templateDepth = max(0, templateDepth-1)
				} else {
					templateDepth++
				}
				continue
			}
			if tag == "script" || tag == "style" || tag == "noscript" {
				if token != html.EndTagToken {
					rawIgnored = tag
				}
				continue
			}
			if templateDepth > 0 {
				continue
			}
			if emailBodyBlock(tag) {
				if err := appendText([]byte{'\n'}); err != nil {
					return "", err
				}
			}
		case html.CommentToken, html.DoctypeToken:
		}
	}
}
func emailBodyBlock(tag string) bool {
	switch tag {
	case "address", "article", "aside", "blockquote", "br", "caption", "dd", "details", "dialog", "div", "dl", "dt", "fieldset", "figcaption", "figure", "footer", "form", "h1", "h2", "h3", "h4", "h5", "h6", "header", "hgroup", "hr", "li", "main", "nav", "ol", "p", "pre", "section", "summary", "table", "tbody", "td", "tfoot", "th", "thead", "tr", "ul":
		return true
	default:
		return false
	}
}

// emailRenditionCatalog keeps ArtifactPublisher's staging and verification flow,
// binding only its final transaction to the exact selected email attachment.
type emailRenditionCatalog struct {
	emailCatalog

	attachmentID, partPath, recipe string
}

func (c emailRenditionCatalog) PublishRenditionAndLexicalHeads(ctx context.Context, attachment store.RenditionAttachmentRecord, head store.RenditionHeadRecord, generationID string) error {
	return c.PublishEmailBody(ctx,
		store.EmailBodyPublication{EmailAttachmentID: c.attachmentID,
			PartPath:              c.partPath,
			BodyRecipeFingerprint: c.recipe,
			RenditionAttachment:   attachment,
			RenditionHead:         head,
			LexicalGenerationID:   generationID})
}
func publishEmailBody(ctx context.Context, catalog emailCatalog, blobs emailBlobs, view store.EmailMetadataView) error {
	recipe, err := EmailBodyRecipeFingerprint(view.Evidence.Recipe)
	if err != nil {
		return err
	}
	var selected *document.EmailAlternativeV1
	if view.Evidence.Inventory != nil {
		for _, message := range view.Evidence.Inventory.Messages {
			if message.Path != "1" || message.SelectedBodyPath == nil {
				continue
			}
			for _, alternative := range message.Alternatives {
				if alternative.PartPath == *message.SelectedBodyPath {
					selected = new(alternative)
					break
				}
			}
		}
	}
	if selected == nil {
		reason := "no_supported_body"
		if view.Evidence.Inventory == nil {
			reason = "inventory_unavailable"
		}
		return catalog.RecordEmailBodyUnavailable(ctx, view.Attachment.ID, recipe, nil, reason)
	}
	ref := selected.Display
	if ref == nil {
		return errors.New("selected email body has no display authority")
	}
	stream, size, err := blobs.OpenStreamContext(ctx, ref.SHA256)
	if err != nil {
		return err
	}
	if size != ref.Size || size > emailBodyMaxBytes {
		return errors.Join(errors.New("selected email body stream size differs from canonical reference"), stream.Close())
	}
	body, readErr := io.ReadAll(io.LimitReader(stream, ref.Size+1))
	if err := errors.Join(readErr, stream.Close()); err != nil {
		return err
	}
	if int64(len(body)) != ref.Size || renditionBytesSHA256(body) != ref.SHA256 {
		return errors.New("selected email body bytes differ from canonical reference")
	}
	text, err := emailBodyText(ctx, selected.Kind, body)
	if err == nil && strings.TrimSpace(text) == "" {
		err = emailBodyUnavailableError("empty_body")
	}
	var staged StagedRendition
	if err == nil {
		staged, err = stageEmailBody(ctx, catalog, view, *selected, text)
	}
	if unavailable, ok := errors.AsType[emailBodyUnavailableError](err); ok {
		return catalog.RecordEmailBodyUnavailable(ctx, view.Attachment.ID, recipe, new(selected.PartPath), string(unavailable))
	}
	if err != nil {
		return err
	}
	if _, err = catalog.MigrateLegacyPlainText(ctx); err != nil {
		return err
	}
	adapter := emailRenditionCatalog{emailCatalog: catalog, attachmentID: view.Attachment.ID, partPath: selected.PartPath, recipe: recipe}
	publisher, err := NewArtifactPublisher(adapter, blobs)
	if err != nil {
		return err
	}
	// A complete projection is immutable. Each attempt gets a fresh generation,
	// and stale publication restages against current authority without reparsing.
	for range 3 {
		staged.LexicalGenerationID = renditionBytesSHA256([]byte(uuid.NewString()))
		_, err = publisher.PublishRendition(ctx, staged)
		if !errors.Is(err, store.ErrLexicalGenerationStale) {
			return err
		}
		// Publisher consumes readers; regenerate only the bounded body candidate.
		staged, err = stageEmailBody(ctx, catalog, view, *selected, text)
		if err != nil {
			return err
		}
	}
	return store.ErrLexicalGenerationStale
}
func stageEmailBody(ctx context.Context, catalog emailCatalog, view store.EmailMetadataView, selected document.EmailAlternativeV1, text string) (StagedRendition, error) {
	profile, err := document.EmailBodyProfileV1(view.Evidence.Recipe)
	if err != nil {
		return StagedRendition{}, err
	}
	canonical, fp, err := document.CanonicalProfile(profile)
	if err != nil {
		return StagedRendition{}, err
	}
	ep, rp, err := document.RenditionExecutionPoliciesForProfileV1(profile)
	if err != nil {
		return StagedRendition{}, err
	}
	units, err := bodyUnits(ctx, text)
	if err != nil {
		return StagedRendition{}, err
	}
	evidence, err := document.NormalizeEvidenceV1(document.SourceEvidenceV1{ContractVersion: document.SourceEvidenceContractV1,
		Completeness: document.EvidenceDegradedProvenance,
		Family:       "text",
		UnitKind:     document.EvidenceUnitGeneric,
		Omissions: []document.SourceEvidenceOmissionV1{{Kind: document.EvidenceOmissionField,
			Field:  "natural_provenance",
			Reason: "Selected body converted to derived generic text blocks; exact MIME authority retained separately."}},
		Units: units},
		ep)
	if err != nil {
		return StagedRendition{}, err
	}
	evidenceBytes, evidenceHash, err := document.MarshalNormalizedEvidenceV1(evidence)
	if err != nil {
		return StagedRendition{}, err
	}
	rendition, err := document.BuildRenditionV1(evidence, rp)
	if err != nil {
		return StagedRendition{}, err
	}
	if err = checkEmailBodyRendition(rendition); err != nil {
		return StagedRendition{}, err
	}
	if err = ctx.Err(); err != nil {
		return StagedRendition{}, err
	}
	recipe, err := EmailBodyRecipeFingerprint(view.Evidence.Recipe)
	if err != nil {
		return StagedRendition{}, err
	}
	receipt := document.EmailBodyReceiptV1{ContractVersion: document.EmailBodyReceiptContractV1,
		SourceSHA256:          view.Version.BlobHash,
		SourceSize:            view.Version.Size,
		EmailGenerationID:     view.Generation.ID,
		EmailChecksum:         view.Generation.Checksum,
		PartPath:              selected.PartPath,
		BodySHA256:            selected.Display.SHA256,
		BodySize:              selected.Display.Size,
		BodyRecipeFingerprint: recipe,
		EvidenceChecksum:      evidenceHash,
		RenditionChecksum:     rendition.Checksum,
		MarkdownChecksum:      rendition.MarkdownChecksum}
	receiptBytes, _, err := document.MarshalEmailBodyReceiptV1(receipt)
	if err != nil {
		return StagedRendition{}, err
	}
	buildID, err := document.EmailBodyBuildID(receipt)
	if err != nil {
		return StagedRendition{}, err
	}
	operation, err := document.EmailBodyOperationID(recipe)
	if err != nil {
		return StagedRendition{}, err
	}
	authorization, err := document.EmailBodyAuthorizationChecksum(receipt.SourceSHA256, receipt.SourceSize, recipe)
	if err != nil {
		return StagedRendition{}, err
	}
	policy := jsontext.Value(`{"roles":[{"max_count":1,"min_count":1,"role":"normalized_evidence"},{"max_count":1,"min_count":1,"role":"sanitized_markdown"}],"version":1}`)
	build := store.RenditionBuildRecord{ID: buildID,
		VaultID:                           catalog.VaultID(),
		SourceSHA256:                      view.Version.BlobHash,
		RenditionRequestFingerprint:       fp.RenditionRequest,
		EvidenceLexicalFingerprint:        fp.EvidenceLexical,
		CapturedArtifactPolicyFingerprint: renditionBytesSHA256(policy),
		CapturedArtifactPolicy:            policy,
		AuthorizationChecksum:             authorization,
		ProviderOperationID:               operation,
		ProviderReceipt:                   receiptBytes,
		EvidenceChecksum:                  evidenceHash,
		RenditionChecksum:                 rendition.Checksum,
		MarkdownChecksum:                  rendition.MarkdownChecksum,
		Completeness:                      rendition.Completeness,
		CompletedAt:                       time.Now().UTC().Format(emailObservationTimeLayout),
		DeclaredArtifactCount:             2}
	var payloads []StagedArtifact
	for _, a := range []struct {
		role string
		data []byte
	}{{"normalized_evidence", evidenceBytes}, {"sanitized_markdown", rendition.Markdown}} {
		checksum := renditionBytesSHA256(a.data)
		id := renditionArtifactID(buildID, a.role, 0, checksum)
		build.Artifacts = append(build.Artifacts,
			store.RenditionArtifactRecord{ID: id,
				Role:     a.role,
				BlobHash: checksum,
				Checksum: checksum,
				Size:     int64(len(a.data)),
				State:    store.RenditionArtifactVerified})
		payloads = append(payloads, StagedArtifact{ID: id, Payload: bytes.NewReader(a.data)})
	}
	for _, u := range rendition.Units {
		build.Units = append(build.Units, store.RenditionUnitRecord{ID: u.ID, EvidenceUnitID: u.EvidenceUnitID, Order: u.Order, Checksum: u.Checksum, HeadingPath: u.HeadingPath, Locator: u.Locator})
	}
	for _, s := range rendition.LexicalSegments {
		build.LexicalSegments = append(build.LexicalSegments,
			store.RenditionLexicalSegmentRecord{ID: s.ID,
				UnitID:    s.UnitID,
				Order:     s.Order,
				CharStart: s.CharStart,
				CharEnd:   s.CharEnd,
				Checksum:  s.Checksum,
				Text:      s.Text})
	}
	for _, w := range rendition.Warnings {
		build.Warnings = append(build.Warnings, w.Code)
	}
	stored, err := catalog.RenditionBuild(ctx, buildID)
	if err == nil {
		build.CompletedAt = stored.CompletedAt
	} else if !errors.Is(err, store.ErrNotFound) {
		return StagedRendition{}, err
	}
	record := store.ProcessingProfileRecord{Fingerprint: fp.Profile,
		CanonicalProfile:               canonical,
		RenditionRequestFingerprint:    fp.RenditionRequest,
		EvidenceLexicalFingerprint:     fp.EvidenceLexical,
		RetentionDisclosureFingerprint: fp.RetentionDisclosure,
		AttachmentPolicyFingerprint:    profile.RetentionDisclosure.AttachmentPolicyFingerprint,
		ConsentFingerprint:             profile.RetentionDisclosure.ConsentFingerprint,
		RenditionDisclosureFingerprint: profile.Rendition.DisclosureFingerprint,
		TrustBoundary:                  profile.RetentionDisclosure.TrustBoundary}
	attachment := store.RenditionAttachmentRecord{ID: store.RenditionAttachmentID(buildID, view.Version.ID, fp.Profile),
		VaultID:          catalog.VaultID(),
		ContentVersionID: view.Version.ID,
		BuildID:          buildID,
		Profile:          record,
		AttachedAt:       time.Now().UTC().Format(emailObservationTimeLayout)}
	return StagedRendition{Rendition: rendition,
			RenditionPolicy: rp,
			Build:           build,
			Attachment:      attachment,
			Head: store.RenditionHeadRecord{ContentVersionID: attachment.ContentVersionID,
				ProcessingProfileFingerprint: fp.Profile,
				AttachmentID:                 attachment.ID,
				PublishedAt:                  attachment.AttachedAt},
			Artifacts: payloads},
		nil
}
func checkEmailBodyRendition(rendition document.RenditionV1) error {
	for _, warning := range rendition.Warnings {
		if warning.Code == "truncated" {
			return emailBodyUnavailableError("body_render_limit")
		}
	}
	return nil
}
