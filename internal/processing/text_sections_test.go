package processing

import (
	"context"
	"crypto/sha256"
	"fmt"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document"
)

func sectionIndex(t *testing.T, source string) textSectionIndex {
	t.Helper()
	index, err := buildTextSectionIndex(t.Context(), []byte(source), newTextSectionParser())
	require.NoError(t, err)
	return index
}

func TestTextSectionsLiteralBoundaries(t *testing.T) {
	t.Parallel()
	source := "intro\n\n# Parent\nbody\n\n## Child\nmore\n\n# Next\nend"
	index := sectionIndex(t, source)
	require.Equal(t, 47, index.chars)
	require.Equal(t, []document.TextOutlineEntry{
		{Start: 0, End: 47}, {Start: 7, End: 37, Level: 1, TitleMarkdown: "Parent"},
		{Start: 22, End: 37, Level: 2, TitleMarkdown: "Child"},
		{Start: 37, End: 47, Level: 1, TitleMarkdown: "Next"},
	}, index.sections)
	for _, tc := range []struct{ start, end, wantStart, wantEnd int }{
		{18, 26, 7, 37}, {30, 34, 22, 37}, {34, 40, 0, 47},
	} {
		page, err := index.outline(document.TextOutlineRequest{
			Citation: document.TextCitation{Start: tc.start, End: tc.end}})
		require.NoError(t, err)
		require.Equal(t, document.TextSectionRange{Start: tc.wantStart, End: tc.wantEnd},
			page.EnclosingSection)
	}
}

func TestTextSectionsMarkdown(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, source string
		want         []document.TextOutlineEntry
	}{
		{"setext", "First\nsecond\n===\nbody", []document.TextOutlineEntry{
			{Start: 0, End: 21}, {Start: 0, End: 21, Level: 1, TitleMarkdown: "First second"}}},
		{"empty repeated skipped", "#\n\n### Same\nx\n\n### Same\ny", []document.TextOutlineEntry{
			{Start: 0, End: 25}, {Start: 0, End: 25, Level: 1},
			{Start: 3, End: 15, Level: 3, TitleMarkdown: "Same"},
			{Start: 15, End: 25, Level: 3, TitleMarkdown: "Same"}}},
		{"unicode CRLF indentation", "é界e\u0301\r\n\r\n   # **Hi** \\*x\\*\r\nz",
			[]document.TextOutlineEntry{
				{Start: 0, End: 28}, {Start: 8, End: 28, Level: 1, TitleMarkdown: "**Hi** \\*x\\*"}}},
		{"containers and code",
			"> # quote\n\n- # list\n\n```\n# fence\n```\n\n    # code\n\n\\# escaped\n\n# Real\nx",
			[]document.TextOutlineEntry{{Start: 0, End: 70},
				{Start: 62, End: 70, Level: 1, TitleMarkdown: "Real"}}},
		{"GFM table", "| A | B |\n| - | - |\n| # nope | ok |\n\n# End\nx",
			[]document.TextOutlineEntry{{Start: 0, End: 44},
				{Start: 37, End: 44, Level: 1, TitleMarkdown: "End"}}},
		{"table before setext marker", "A | B\n- | -\nx | y\n---\n\n# End\nz",
			[]document.TextOutlineEntry{{Start: 0, End: 30},
				{Start: 23, End: 30, Level: 1, TitleMarkdown: "End"}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, tc.want, sectionIndex(t, tc.source).sections)
		})
	}
}

func TestTextSectionsNormalizedText(t *testing.T) {
	t.Parallel()
	ep, err := document.NewEvidencePolicy(100_000)
	require.NoError(t, err)
	evidence, err := document.NormalizeEvidenceV1(document.SourceEvidenceV1{
		ContractVersion: document.SourceEvidenceContractV1, Completeness: document.EvidenceComplete,
		Family: "text", UnitKind: document.EvidenceUnitSection, Units: []document.SourceEvidenceUnitV1{
			{Order: 0, Locator: document.SourceEvidenceLocatorV1{Kind: document.EvidenceLocatorSection,
				IndexOrigin: document.EvidenceIndexOriginNone}, Text: "# comment\n\nvalue"},
			{Order: 1, Locator: document.SourceEvidenceLocatorV1{Kind: document.EvidenceLocatorSection,
				IndexOrigin: document.EvidenceIndexOriginNone}, Text: "tail"},
		}}, ep)
	require.NoError(t, err)
	rp, err := document.NewRenditionPolicy(document.RenditionLimits{
		MaxDocumentChars: 100_000, MaxUnitRunes: 100_000, MaxSegmentRunes: 4000})
	require.NoError(t, err)
	rendition, err := document.BuildRenditionV1(evidence, rp)
	require.NoError(t, err)
	require.Equal(t, "# comment\nvalue\n\n---\n\ntail\n", string(rendition.Markdown))
	index := sectionIndex(t, string(rendition.Markdown))
	require.Len(t, index.sections, 2)
	require.Equal(t, "comment", index.sections[1].TitleMarkdown)
}

func TestTextSectionsOutline(t *testing.T) {
	t.Parallel()
	var source strings.Builder
	for i := range 51 {
		fmt.Fprintf(&source, "# H%02d\nx\n", i)
	}
	index := sectionIndex(t, source.String())
	request := document.TextOutlineRequest{Citation: document.TextCitation{Start: 405, End: 406}}
	page, err := index.outline(request)
	require.NoError(t, err)
	require.Equal(t, 52, page.Total)
	require.Len(t, page.Sections, 50)
	require.Equal(t, 50, *page.NextOffset)
	require.Equal(t, document.TextSectionRange{Start: 400, End: 408}, page.EnclosingSection)
	require.Equal(t, document.TextOutlineEntry{Start: 0, End: 408}, page.Sections[0])
	request.Offset = 50
	page, err = index.outline(request)
	require.NoError(t, err)
	require.Equal(t, []document.TextOutlineEntry{
		{Start: 392, End: 400, Level: 1, TitleMarkdown: "H49"},
		{Start: 400, End: 408, Level: 1, TitleMarkdown: "H50"}}, page.Sections)
	require.Nil(t, page.NextOffset)
	request.Offset = 52
	page, err = index.outline(request)
	require.NoError(t, err)
	require.NotNil(t, page.Sections)
	require.Empty(t, page.Sections)
	require.Nil(t, page.NextOffset)
	request.Offset = 53
	_, err = index.outline(request)
	require.ErrorIs(t, err, document.ErrInvalidTextNavigation)
	for _, size := range []int{160, 161} {
		index = sectionIndex(t, "# "+strings.Repeat("界", size))
		require.Equal(t, 0, index.sections[1].Start)
		require.Equal(t, index.sections[0].End, index.sections[1].End)
		require.Equal(t, strings.Repeat("界", 160), index.sections[1].TitleMarkdown)
		require.Equal(t, size == 161, index.sections[1].TitleTruncated)
	}
}

func TestTextSectionsPages(t *testing.T) {
	t.Parallel()
	source := "# Long\r\n" + strings.Repeat("é界e\u0301\r\n", 6000) + "\n# Next\ntail"
	index := sectionIndex(t, source)
	request := document.TextSectionRequest{Citation: document.TextCitation{Start: 1, End: 2},
		Section: document.TextSectionRange{Start: 0, End: 36009}, MaxChars: 16000}
	var combined strings.Builder
	for i, size := range []int{16000, 9000, 16000} {
		request.MaxChars = size
		page, err := index.page(t.Context(), []byte(source), request)
		require.NoError(t, err)
		require.Equal(t, i == 2, page.Complete)
		require.Equal(t, request.Offset, page.ActualStart)
		require.Equal(t, utf8.RuneCountInString(page.Text), page.ActualEnd-page.ActualStart)
		require.Equal(t, len(page.Text), page.TextBytes)
		require.Equal(t, fmt.Sprintf("%x", sha256.Sum256([]byte(page.Text))), page.TextSHA256)
		require.Greater(t, page.NextOffset, request.Offset)
		combined.WriteString(page.Text)
		request.Offset = page.NextOffset
	}
	require.Equal(t, "# Long\r\n"+strings.Repeat("é界e\u0301\r\n", 6000)+"\n", combined.String())
	page, err := index.page(t.Context(), []byte(source), request)
	require.NoError(t, err)
	require.True(t, page.Complete)
	require.Empty(t, page.Text)
	require.Zero(t, page.TextBytes)
	require.Equal(t, request.Offset, page.NextOffset)
	require.Equal(t, "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855",
		page.TextSHA256)
	request.Offset++
	_, err = index.page(t.Context(), []byte(source), request)
	require.ErrorIs(t, err, document.ErrInvalidTextNavigation)
	request.Offset = 0
	request.Section.End--
	_, err = index.page(t.Context(), []byte(source), request)
	require.ErrorIs(t, err, document.ErrInvalidTextNavigation)
	request.Section = document.TextSectionRange{Start: 36009, End: 36020}
	page, err = index.page(t.Context(), []byte(source), request)
	require.NoError(t, err)
	require.Equal(t, "# Next\ntail", page.Text)
}

func TestTextSectionsLimits(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, source string
		ok           bool
	}{
		{"lines at cap", strings.Repeat("\n", 65536), true},
		{"lines over cap", strings.Repeat("\n", 65537), false},
		{"headings at cap", strings.Repeat("# x\n", 4096), true},
		{"headings over cap", strings.Repeat("# x\n", 4097), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := buildTextSectionIndex(t.Context(), []byte(tc.source), newTextSectionParser())
			if tc.ok {
				require.NoError(t, err)
			} else {
				require.ErrorIs(t, err, document.ErrTextNavigationLimit)
			}
		})
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err := buildTextSectionIndex(ctx, []byte("# x\n"), newTextSectionParser())
	require.ErrorIs(t, err, context.Canceled)
}

//nolint:paralleltest // AllocsPerRun forbids running during parallel tests.
func TestTextSectionsInlineMarkerAllocations(t *testing.T) {
	source := []byte("# Title\n" + strings.Repeat("[a](", 4096))
	var index textSectionIndex
	var err error
	allocs := testing.AllocsPerRun(1, func() {
		index, err = buildTextSectionIndex(t.Context(), source, newTextSectionParser())
	})
	require.NoError(t, err)
	require.Equal(t, []document.TextOutlineEntry{
		{Start: 0, End: 16392}, {Start: 0, End: 16392, Level: 1, TitleMarkdown: "Title"},
	}, index.sections)
	// A heading and one paragraph must not allocate a syntax node per inline marker.
	require.Less(t, allocs, 1000.0)
}

func BenchmarkTextSectionsInlineMarkers(b *testing.B) {
	for _, marker := range []struct{ name, text string }{
		{"links", "[a]("}, {"emphasis", "*a"}, {"brackets", "["},
	} {
		for _, size := range []int{16 << 10, 128 << 10, 4 << 20} {
			b.Run(fmt.Sprintf("%s/%dKiB", marker.name, size>>10), func(b *testing.B) {
				data := []byte("# Title\n" + strings.Repeat(marker.text, (size-8)/len(marker.text)))
				b.SetBytes(int64(len(data)))
				b.ReportAllocs()
				b.ResetTimer()
				for b.Loop() {
					index, err := buildTextSectionIndex(b.Context(), data, newTextSectionParser())
					if err != nil || len(index.sections) != 2 || index.sections[1].TitleMarkdown != "Title" {
						b.Fatalf("unexpected outline: %#v, %v", index, err)
					}
				}
			})
		}
	}
}
