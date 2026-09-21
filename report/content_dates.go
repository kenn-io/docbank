package report

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json/v2"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"
)

const (
	maxDocumentTextBytes = 16 << 20
	maxDocumentDates     = 256
	maxQuoteBytes        = 512
	maxPageMapBytes      = 1 << 20
)

const monthPattern = `(?:January|February|March|April|May|June|July|August|September|October|November|December|Jan|Feb|Mar|Apr|Jun|Jul|Aug|Sep|Sept|Oct|Nov|Dec)`
const dateTokenPattern = `(?:\d{4}-\d{2}-\d{2}|\d{1,2}/\d{1,2}/\d{4}|` + monthPattern + `\s+\d{1,2},?\s+\d{4})`

var (
	labeledDatePattern = regexp.MustCompile(`(?i)\b(document\s+dated|date\s+of\s+document|signed\s+on|executed\s+on|effective\s+date|commencement\s+date|expires\s+on)\s*[:,-]?\s*(` + dateTokenPattern + `)`)
	bareDatePattern    = regexp.MustCompile(`(?i)\b` + dateTokenPattern + `\b`)
)

type pageSpan struct {
	StartByte int64 `json:"start_byte"`
	EndByte   int64 `json:"end_byte"`
	Page      int   `json:"page"`
}

type byteSpan struct{ start, end int }

// ExtractContentDates scans exact, already verified native or rendition text.
// pageMap is an optional bounded JSON array of byte spans with one-based pages.
// The function does no I/O and never requests OCR or another provider.
func ExtractContentDates(ctx context.Context, budget Budget, identity Identity, binding TextBinding, text []byte, pageMap string) (_ []DateCandidate, err error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if budget == nil {
		return nil, errors.New("missing report budget")
	}
	if len(text) > maxDocumentTextBytes {
		return nil, fmt.Errorf("text exceeds %d-byte report limit", maxDocumentTextBytes)
	}
	if binding.Document != identity || binding.Size != int64(len(text)) {
		return nil, errors.New("text binding does not match captured document")
	}
	digest := sha256.Sum256(text)
	textSHA := hex.EncodeToString(digest[:])
	switch binding.Kind {
	case "native":
		if binding.Native == nil || binding.Native.TextSHA256 != textSHA || binding.Native.SearchableVersionID != identity.VersionID {
			return nil, errors.New("native text binding differs from captured bytes")
		}
	case "rendition":
		if binding.Native != nil || binding.ArtifactSHA256 != textSHA || binding.RenditionID == "" {
			return nil, errors.New("rendition text binding differs from captured bytes")
		}
	default:
		return nil, fmt.Errorf("unknown text binding kind %q", binding.Kind)
	}
	pages, err := parsePageMap(pageMap, int64(len(text)))
	if err != nil {
		return nil, err
	}
	releaseScratch, err := budget.Reserve(ctx, int64(len(text)))
	if err != nil {
		return nil, err
	}
	defer releaseScratch()
	result := make([]DateCandidate, 0, 4)
	releases := make([]func(), 0, 4)
	defer func() {
		if err != nil {
			for _, release := range releases {
				release()
			}
		}
	}()
	add := func(start, end int, raw, role, confidence string) error {
		if len(result) == maxDocumentDates {
			return fmt.Errorf("document has more than %d date candidates", maxDocumentDates)
		}
		if end-start > maxQuoteBytes || start < 0 || end > len(text) || start >= end {
			return fmt.Errorf("date evidence quote exceeds %d bytes", maxQuoteBytes)
		}
		quote := string(text[start:end])
		release, reserveErr := budget.Reserve(ctx, int64(2048+len(raw)+len(quote)))
		if reserveErr != nil {
			return reserveErr
		}
		releases = append(releases, release)
		value, rejection := normalizeContentToken(raw)
		locator := Locator{
			EvidenceID: binding.GenerationID, EvidenceSHA256: textSHA,
			RenditionID: binding.RenditionID, TextSHA256: textSHA,
			Page:      pageForSpan(pages, int64(start), int64(end)),
			StartByte: int64(start), EndByte: int64(end), Quote: quote,
		}
		candidate := DateCandidate{
			Document: identity, Role: role, SourceClass: "content",
			Raw: raw, Value: value, Precision: "date", Timezone: "date_only",
			Confidence: confidence, Locator: locator, Rejection: rejection,
		}
		candidate.ID = contentCandidateID(candidate)
		result = append(result, candidate)
		return nil
	}
	labeled := make([]byteSpan, 0, 4)
	for cursor := 0; cursor < len(text); {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		match := labeledDatePattern.FindSubmatchIndex(text[cursor:])
		if match == nil {
			break
		}
		start, end := cursor+match[0], cursor+match[1]
		label := strings.ToLower(strings.Join(strings.Fields(string(text[cursor+match[2]:cursor+match[3]])), " "))
		role := roleForDateLabel(label)
		raw := string(text[cursor+match[4] : cursor+match[5]])
		confidence := "explicit_label"
		if strings.Contains(raw, "/") {
			confidence = "ambiguous"
		}
		if err := add(start, end, raw, role, confidence); err != nil {
			return nil, err
		}
		labeled = append(labeled, byteSpan{start, end})
		cursor = end
	}
	for cursor := 0; cursor < len(text); {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		match := bareDatePattern.FindIndex(text[cursor:])
		if match == nil {
			break
		}
		start, end := cursor+match[0], cursor+match[1]
		cursor = end
		covered := false
		for _, span := range labeled {
			if start >= span.start && end <= span.end {
				covered = true
				break
			}
		}
		if covered {
			continue
		}
		raw := string(text[start:end])
		if err := add(start, end, raw, "unclassified", "unclassified"); err != nil {
			return nil, err
		}
	}
	return result, nil
}

func roleForDateLabel(label string) string {
	switch label {
	case "document dated", "date of document":
		return "document_date"
	case "signed on", "executed on":
		return "signed"
	case "effective date", "commencement date":
		return "effective"
	case "expires on":
		return "expiry"
	default:
		return "unclassified"
	}
}

func normalizeContentToken(raw string) (value, rejection string) {
	if date, err := parseISODate(raw); err == nil {
		return date.Format(time.DateOnly), ""
	}
	if strings.Contains(raw, "/") {
		return "", "ambiguous_numeric_date"
	}
	clean := strings.ReplaceAll(strings.TrimSpace(raw), ",", "")
	for _, layout := range []string{"January 2 2006", "Jan 2 2006"} {
		if date, err := time.Parse(layout, clean); err == nil {
			return date.Format(time.DateOnly), ""
		}
	}
	return "", "invalid_calendar_date"
}

func contentCandidateID(candidate DateCandidate) string {
	hash := sha256.New()
	for _, value := range []string{
		strconv.FormatInt(candidate.Document.NodeID, 10), candidate.Document.VersionID, candidate.Document.SHA256,
		candidate.Role, candidate.Raw, candidate.Locator.TextSHA256,
		strconv.FormatInt(candidate.Locator.StartByte, 10), strconv.FormatInt(candidate.Locator.EndByte, 10),
	} {
		_, _ = fmt.Fprintf(hash, "%d:%s", len(value), value)
	}
	return hex.EncodeToString(hash.Sum(nil))
}

func parsePageMap(encoded string, textBytes int64) ([]pageSpan, error) {
	if encoded == "" {
		return nil, nil
	}
	if len(encoded) > maxPageMapBytes {
		return nil, errors.New("text page map exceeds limit")
	}
	var pages []pageSpan
	if err := json.Unmarshal([]byte(encoded), &pages, json.RejectUnknownMembers(true)); err != nil {
		return nil, fmt.Errorf("invalid text page map: %w", err)
	}
	if len(pages) > 4096 {
		return nil, errors.New("text page map has too many spans")
	}
	previousEnd := int64(0)
	for _, page := range pages {
		if page.Page <= 0 || page.StartByte < previousEnd || page.EndByte <= page.StartByte || page.EndByte > textBytes {
			return nil, errors.New("invalid text page span")
		}
		previousEnd = page.EndByte
	}
	return pages, nil
}

func pageForSpan(pages []pageSpan, start, end int64) int {
	for _, page := range pages {
		if start >= page.StartByte && end <= page.EndByte {
			return page.Page
		}
	}
	return 0
}
