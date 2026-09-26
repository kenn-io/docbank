package api

import (
	"encoding/json/v2"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document"
)

func TestOverlappingConnectionInputsSelectsEveryExactStoredSegment(t *testing.T) {
	body := connectionBody{bytes: []byte("alpha beta"), unitKeys: []string{"unit-1"},
		navByKey: map[string]int{"unit-1": 0}, frontmatter: document.RenditionFrontMatterV1{
			Navigation: document.RenditionMarkdownNavigationV1{Entries: []document.RenditionNavigationEntryV1{{Key: "unit-1", Byte: 0}}},
		}}
	inputs := []document.GeneratedEmbeddingInput{
		{Key: "alpha-input", Content: "alpha", SourceSpan: document.ChunkSpan{UnitIndex: 0, CharStart: 0, CharEnd: 5}},
		{Key: "beta-input", Content: "beta", SourceSpan: document.ChunkSpan{UnitIndex: 0, CharStart: 6, CharEnd: 10}},
	}
	all := overlappingConnectionInputs(inputs, body, 0, len(body.bytes))
	require.Len(t, all, 2)
	assert.Equal(t, "alpha-input", all[0].Key)
	assert.Equal(t, "beta-input", all[1].Key)
	passage := overlappingConnectionInputs(inputs, body, 0, 5)
	require.Len(t, passage, 1)
	assert.Equal(t, "alpha-input", passage[0].Key)
}

func TestLexicalDuplicateRequiresExactQuoteBeforeCount(t *testing.T) {
	candidate := ConnectionSuggestion{DuplicateMembers: []ConnectionDuplicateMember{}}
	member := ConnectionDuplicateMember{TargetNodeID: 7, TargetQuote: "exact quote"}
	assert.False(t, appendLexicalDuplicate(&candidate, "exact quote",
		connectionBody{bytes: []byte("a different retained rendition")}, member))
	assert.Zero(t, candidate.DuplicateCount)
	assert.Empty(t, candidate.DuplicateMembers)

	assert.True(t, appendLexicalDuplicate(&candidate, "exact quote",
		connectionBody{bytes: []byte("prefix exact quote suffix")}, member))
	assert.Equal(t, 1, candidate.DuplicateCount)
	require.Len(t, candidate.DuplicateMembers, 1)
	assert.Equal(t, int64(7), candidate.DuplicateMembers[0].TargetNodeID)
}

func TestConnectionReportTruncatesWholeCandidateGroups(t *testing.T) {
	complete := ConnectionSuggestion{ID: "complete", SourceInputID: "seed",
		DuplicateMembers: []ConnectionDuplicateMember{{ID: "first", SourceInputID: "seed"},
			{ID: "second", SourceInputID: "seed"}}, DuplicateCount: 2}
	oversized := ConnectionSuggestion{ID: "oversized", SourceInputID: "seed",
		DuplicateMembers: []ConnectionDuplicateMember{{ID: "large", SourceInputID: "seed",
			TargetQuote: strings.Repeat("x", maxConnectionSuggestionReportBytes)}},
		DuplicateCount: 1}
	report := ConnectionSuggestionReport{State: "ready", Method: "semantic",
		SeedSegments: []ConnectionSeedSegment{{InputID: "seed", Quote: "seed"}},
		Candidates:   []ConnectionSuggestion{complete, oversized}}
	bounded, err := boundConnectionSuggestionReport(report)
	require.NoError(t, err)
	assert.Equal(t, "ready", bounded.State)
	assert.True(t, bounded.Truncated)
	assert.Equal(t, 1, bounded.CandidateCount)
	require.Len(t, bounded.Candidates, 1)
	assert.Equal(t, complete, bounded.Candidates[0], "duplicate evidence is never split")
	encoded, err := json.Marshal(bounded)
	require.NoError(t, err)
	assert.LessOrEqual(t, len(encoded), maxConnectionSuggestionReportBytes)
}

func TestConnectionReportTruncatesOversizedSingleGroupAndSeedEvidence(t *testing.T) {
	for _, test := range []struct {
		name   string
		report ConnectionSuggestionReport
	}{
		{name: "one candidate", report: ConnectionSuggestionReport{State: "ready", Method: "semantic",
			SeedSegments: []ConnectionSeedSegment{{InputID: "seed", Quote: "seed"}},
			Candidates: []ConnectionSuggestion{{ID: "oversized", SourceInputID: "seed",
				DuplicateMembers: []ConnectionDuplicateMember{{SourceInputID: "seed",
					TargetQuote: strings.Repeat("x", maxConnectionSuggestionReportBytes)}},
				DuplicateCount: 1}}}},
		{name: "seed evidence", report: ConnectionSuggestionReport{State: "ready", Method: "semantic",
			SeedSegments: []ConnectionSeedSegment{{InputID: "seed", Quote: strings.Repeat("x", maxConnectionSuggestionReportBytes)}},
			Candidates:   []ConnectionSuggestion{{ID: "small", SourceInputID: "seed", DuplicateMembers: []ConnectionDuplicateMember{}}}}},
	} {
		t.Run(test.name, func(t *testing.T) {
			bounded, err := boundConnectionSuggestionReport(test.report)
			require.NoError(t, err)
			assert.Equal(t, "ready", bounded.State)
			assert.True(t, bounded.Truncated)
			assert.Zero(t, bounded.CandidateCount)
			assert.Empty(t, bounded.Candidates)
			encoded, err := json.Marshal(bounded)
			require.NoError(t, err)
			assert.LessOrEqual(t, len(encoded), maxConnectionSuggestionReportBytes)
		})
	}
}

func TestConnectionReportRetainsEveryReturnedPairInput(t *testing.T) {
	report := ConnectionSuggestionReport{State: "ready", Method: "semantic",
		SeedSegments: []ConnectionSeedSegment{{InputID: "primary", Quote: "first"},
			{InputID: "duplicate", Quote: "second"},
			{InputID: "unused", Quote: strings.Repeat("x", maxConnectionSuggestionReportBytes)}},
		Candidates: []ConnectionSuggestion{{ID: "group", SourceInputID: "primary",
			DuplicateMembers: []ConnectionDuplicateMember{{ID: "member", SourceInputID: "duplicate"}}, DuplicateCount: 1}}}
	bounded, err := boundConnectionSuggestionReport(report)
	require.NoError(t, err)
	assert.True(t, bounded.Truncated)
	assert.Equal(t, 1, bounded.CandidateCount)
	assert.Equal(t, 2, bounded.SeedSegmentCount)
	require.Len(t, bounded.SeedSegments, 2)
	assert.Equal(t, []string{"primary", "duplicate"},
		[]string{bounded.SeedSegments[0].InputID, bounded.SeedSegments[1].InputID})
	require.Len(t, bounded.Candidates, 1)
	assert.Equal(t, 1, bounded.Candidates[0].DuplicateCount)
	require.Len(t, bounded.Candidates[0].DuplicateMembers, 1)
}
