package report

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"testing"
)

func nativeDateBinding(identity Identity, text []byte) TextBinding {
	digest := sha256.Sum256(text)
	return TextBinding{
		Kind: "native", Document: identity, Size: int64(len(text)),
		Native: &NativeText{TextSHA256: hex.EncodeToString(digest[:]), Status: "complete", SearchableVersionID: identity.VersionID},
	}
}

func TestContentDatesKeepSemanticRoleAndExactByteSpan(t *testing.T) {
	ctx := context.Background()
	budget := NewBudget(1 << 20)
	defer func() { _ = budget.Close() }()
	identity := Identity{NodeID: 1, VersionID: "v1", SHA256: strings.Repeat("a", 64)}
	text := []byte("Document dated 2024-06-03. Effective date: 2024-07-01.")
	got, err := ExtractContentDates(ctx, budget, identity, nativeDateBinding(identity, text), text, "")
	if err != nil || len(got) != 2 {
		t.Fatalf("candidates=%+v err=%v", got, err)
	}
	if got[0].Role != "document_date" || got[1].Role != "effective" {
		t.Fatalf("lost date roles: %+v", got)
	}
	for _, candidate := range got {
		if string(text[candidate.Locator.StartByte:candidate.Locator.EndByte]) != candidate.Locator.Quote {
			t.Fatalf("locator does not reproduce quote: %+v", candidate)
		}
		if candidate.SourceClass != "content" || candidate.Locator.TextSHA256 != nativeDateBinding(identity, text).Native.TextSHA256 {
			t.Fatalf("source binding lost: %+v", candidate)
		}
	}
}

func TestContentDatesLocateOCRAcrossMultilineAndUnicode(t *testing.T) {
	budget := NewBudget(1 << 20)
	defer func() { _ = budget.Close() }()
	identity := Identity{NodeID: 2, VersionID: "v2", SHA256: strings.Repeat("b", 64)}
	text := []byte("Résumé\nSigned on\nJune 3, 2024\n")
	digest := sha256.Sum256(text)
	mapJSON := fmt.Sprintf(`[{"start_byte":0,"end_byte":%d,"page":7}]`, len(text))
	binding := TextBinding{Kind: "rendition", Document: identity, Size: int64(len(text)), RenditionID: "ocr-r1", ArtifactSHA256: hex.EncodeToString(digest[:])}
	got, err := ExtractContentDates(context.Background(), budget, identity, binding, text, mapJSON)
	if err != nil || len(got) != 1 {
		t.Fatalf("candidates=%+v err=%v", got, err)
	}
	if got[0].Role != "signed" || got[0].Value != "2024-06-03" || got[0].Locator.Page != 7 || got[0].Locator.RenditionID != "ocr-r1" {
		t.Fatalf("OCR locator or role lost: %+v", got[0])
	}
	if got[0].Locator.StartByte != int64(bytes.Index(text, []byte("Signed"))) {
		t.Fatalf("locator used rune instead of byte offset: %+v", got[0].Locator)
	}
}

func TestContentDatesKeepAmbiguousAndBareDatesForReview(t *testing.T) {
	budget := NewBudget(1 << 20)
	defer func() { _ = budget.Close() }()
	identity := Identity{NodeID: 1, VersionID: "v1"}
	text := []byte("Document dated 03/04/2020. 2024-08-09 is mentioned elsewhere.")
	got, err := ExtractContentDates(context.Background(), budget, identity, nativeDateBinding(identity, text), text, "")
	if err != nil || len(got) != 2 {
		t.Fatalf("candidates=%+v err=%v", got, err)
	}
	if got[0].Role != "document_date" || got[0].Rejection != "ambiguous_numeric_date" || got[1].Role != "unclassified" {
		t.Fatalf("date classification changed: %+v", got)
	}
}

func TestContentDatesParseSeptWithoutChangingEvidence(t *testing.T) {
	for _, raw := range []string{"Sept 2, 2024", "SEPT 2, 2024", "Sept\t2,\n2024", "Sept 31, 2024"} {
		t.Run(raw, func(t *testing.T) {
			budget := NewBudget(1 << 20)
			defer func() { _ = budget.Close() }()
			identity := Identity{NodeID: 1, VersionID: "v1"}
			text := []byte("Document dated " + raw)
			got, err := ExtractContentDates(t.Context(), budget, identity, nativeDateBinding(identity, text), text, "")
			if err != nil || len(got) != 1 {
				t.Fatalf("candidates=%+v err=%v", got, err)
			}
			if got[0].Raw != raw || got[0].Locator.Quote != string(text) {
				t.Fatalf("original evidence changed: %+v", got[0])
			}
			selection, err := SelectDate("document", got, nil, Request{Timezone: "UTC"})
			if raw == "Sept 31, 2024" {
				if got[0].Rejection != "invalid_calendar_date" || !errors.Is(err, ErrUnusableDate) {
					t.Fatalf("invalid calendar date accepted: %+v err=%v", got[0], err)
				}
			} else if err != nil || selection.Date != "2024-09-02" {
				t.Fatalf("valid September date rejected: %+v err=%v", got[0], err)
			}
		})
	}
}

func TestContentDatesFailInsteadOfTruncatingOrSubstituting(t *testing.T) {
	identity := Identity{NodeID: 1, VersionID: "v1"}
	for _, tc := range []struct {
		name    string
		text    []byte
		binding func([]byte) TextBinding
		ctx     func() context.Context
	}{
		{"too many candidates", []byte(strings.Repeat("Document dated 2024-06-03. ", 257)), func(text []byte) TextBinding { return nativeDateBinding(identity, text) }, context.Background},
		{"oversized text", make([]byte, 16<<20+1), func(text []byte) TextBinding { return nativeDateBinding(identity, text) }, context.Background},
		{"mismatched digest", []byte("Document dated 2024-06-03"), func(text []byte) TextBinding {
			binding := nativeDateBinding(identity, text)
			binding.Native.TextSHA256 = strings.Repeat("0", 64)
			return binding
		}, context.Background},
		{"canceled", []byte("Document dated 2024-06-03"), func(text []byte) TextBinding { return nativeDateBinding(identity, text) }, func() context.Context {
			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			return ctx
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			budget := NewBudget(32 << 20)
			defer func() { _ = budget.Close() }()
			if got, err := ExtractContentDates(tc.ctx(), budget, identity, tc.binding(tc.text), tc.text, ""); err == nil {
				t.Fatalf("accepted incomplete extraction: %+v", got)
			}
		})
	}
}

func TestContentDateLimitIdentifiesDocument(t *testing.T) {
	budget := NewBudget(1 << 20)
	defer func() { _ = budget.Close() }()
	identity := Identity{NodeID: 42, VersionID: "version-42"}
	text := []byte(strings.Repeat("Document dated 2024-06-03. ", 257))
	_, err := ExtractContentDates(t.Context(), budget, identity, nativeDateBinding(identity, text), text, "")
	limit, ok := errors.AsType[*ContentDateLimitError](err)
	if !ok || limit.Document != identity || limit.Limit != 256 || !strings.Contains(err.Error(), "document 42") {
		t.Fatalf("candidate limit must identify its document: %v", err)
	}
	if budget.Used() != 0 {
		t.Fatalf("failed extraction retained %d bytes", budget.Used())
	}
}
