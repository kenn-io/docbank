package embedfit_test

import (
	"errors"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/kit/embedconfig"
	"go.kenn.io/kit/embedfit"
)

func TestFitKeepsAShortSourceWhole(t *testing.T) {
	got, err := embedfit.Fit("alpha beta", "", "", words{}, policy(8, 0, 0, embedconfig.TruncationReject))
	require.NoError(t, err)
	require.Len(t, got.Spans, 1)
	assert.Equal(t, "alpha beta", got.Spans[0].Text)
	assert.Equal(t, 0, got.Spans[0].RuneStart)
	assert.Equal(t, utf8.RuneCountInString("alpha beta"), got.Spans[0].RuneEnd)
	assert.Equal(t, len("alpha beta"), got.Spans[0].ByteEnd)
	assert.False(t, got.Spans[0].Truncated)
	assert.False(t, got.TailDropped)

	blank, err := embedfit.Fit(" \u200b", "doc:", "", words{}, policy(8, 0, 0, embedconfig.TruncationReject))
	require.NoError(t, err)
	assert.Empty(t, blank.Spans)
}

func TestFitTrimsBlankTextAroundSpans(t *testing.T) {
	source := " \u200b alpha beta\n\u200b"
	got, err := embedfit.Fit(source, "", "", words{}, policy(8, 0, 0, embedconfig.TruncationReject))
	require.NoError(t, err)
	require.Len(t, got.Spans, 1)
	span := got.Spans[0]
	assert.Equal(t, "alpha beta", span.Text)
	assert.Equal(t, span.Text, source[span.ByteStart:span.ByteEnd])
	assert.Equal(t, 3, span.RuneStart)
	assert.Equal(t, 13, span.RuneEnd)

	// A run of invisible format characters between two windows is skipped,
	// not emitted as a span the embed client would reject as empty.
	source = "aaaa" + strings.Repeat("\u200b", 8) + "bbbb"
	got, err = embedfit.Fit(source, "", "", runes{}, policy(4, 0, 0, embedconfig.TruncationDropTail))
	require.NoError(t, err)
	require.Len(t, got.Spans, 2)
	assert.Equal(t, "aaaa", got.Spans[0].Text)
	assert.Equal(t, "bbbb", got.Spans[1].Text)
	assert.Equal(t, 12, got.Spans[1].RuneStart)
	assert.False(t, got.TailDropped)
}

func TestFitEndsASentenceAtTheWindowEdge(t *testing.T) {
	// The window "Hello world." fills the budget exactly. The space after it
	// is outside the window, but the period still ends a sentence.
	source := "Hello world. Next one."
	got, err := embedfit.Fit(source, "", "", runes{}, policy(12, 0, 0, embedconfig.TruncationReject))
	require.NoError(t, err)
	require.Len(t, got.Spans, 2)
	assert.Equal(t, "Hello world.", got.Spans[0].Text)
	assert.False(t, got.Spans[0].Truncated)
	assert.Equal(t, "Next one.", got.Spans[1].Text)
	assert.Equal(t, len("Hello world. "), got.Spans[1].ByteStart)
	assertBudget(t, got, "", "", runes{}, 12)
}

func TestFitPrefersAParagraphBoundary(t *testing.T) {
	source := "aaaa bbbb\n\ncccc dddd"
	got, err := embedfit.Fit(source, "", "", words{}, policy(2, 0, 0, embedconfig.TruncationDropTail))
	require.NoError(t, err)
	require.GreaterOrEqual(t, len(got.Spans), 2)
	assert.Equal(t, "aaaa bbbb", got.Spans[0].Text)
	assert.False(t, got.Spans[0].Truncated)
	assert.Equal(t, "cccc dddd", got.Spans[1].Text)
	assertBudget(t, got, "", "", words{}, 2)
}

func TestFitCountsPrefixAndSuffix(t *testing.T) {
	got, err := embedfit.Fit("aa bb cc", "P Q ", " END", words{}, policy(4, 0, 0, embedconfig.TruncationDropTail))
	require.NoError(t, err)
	require.NotEmpty(t, got.Spans)
	assert.Equal(t, "aa", strings.TrimSpace(got.Spans[0].Text))
	assertBudget(t, got, "P Q ", " END", words{}, 4)
	prepared := got.Prepared()
	assert.Equal(t, "P Q "+got.Spans[0].Text+" END", prepared[0].Text)
	assert.Equal(t, got.Spans[0].ByteStart, prepared[0].Span.ByteStart)
	assert.Equal(t, got.Spans[0].RuneEnd, prepared[0].Span.RuneEnd)
}

func TestFitOverlapsSourceTokens(t *testing.T) {
	got, err := embedfit.Fit("abcdefghij", "", "", runes{}, policy(4, 2, 0, embedconfig.TruncationDropTail))
	require.NoError(t, err)
	require.GreaterOrEqual(t, len(got.Spans), 2)
	assert.Equal(t, "abcd", got.Spans[0].Text)
	assert.True(t, got.Spans[0].Truncated)
	assert.Less(t, got.Spans[1].RuneStart, got.Spans[0].RuneEnd)
	assert.Greater(t, got.Spans[1].RuneStart, got.Spans[0].RuneStart)
	assert.True(t, covers("abcdefghij", got))
	assert.False(t, got.TailDropped)
	assertBudget(t, got, "", "", runes{}, 4)
}

func TestFitRejectsHardCutsAndDroppedTails(t *testing.T) {
	_, err := embedfit.Fit("abcdefghij", "", "", runes{}, policy(4, 0, 0, embedconfig.TruncationReject))
	require.ErrorIs(t, err, embedfit.ErrHardCut)

	source := "aaaa bbbb\n\ncccc dddd"
	_, err = embedfit.Fit(source, "", "", words{}, policy(2, 0, 1, embedconfig.TruncationReject))
	require.ErrorIs(t, err, embedfit.ErrTailDropped)

	got, err := embedfit.Fit(source, "", "", words{}, policy(2, 0, 1, embedconfig.TruncationDropTail))
	require.NoError(t, err)
	require.Len(t, got.Spans, 1)
	assert.True(t, got.TailDropped)
	assert.Equal(t, "aaaa bbbb", got.Spans[0].Text)
}

func TestFitRecordsUnicodeByteOffsets(t *testing.T) {
	got, err := embedfit.Fit("héllo", "", "", runes{}, policy(3, 0, 0, embedconfig.TruncationDropTail))
	require.NoError(t, err)
	require.NotEmpty(t, got.Spans)
	assert.Equal(t, "hél", got.Spans[0].Text)
	assert.Equal(t, 0, got.Spans[0].RuneStart)
	assert.Equal(t, 3, got.Spans[0].RuneEnd)
	assert.Equal(t, len("hél"), got.Spans[0].ByteEnd)
	assert.Greater(t, got.Spans[0].ByteEnd, got.Spans[0].RuneEnd)
}

func TestFitReportsTokenizerFailures(t *testing.T) {
	boom := errors.New("tokenizer unavailable")
	_, err := embedfit.Fit("alpha", "", "", failTokenizer{boom}, policy(4, 0, 0, embedconfig.TruncationReject))
	require.ErrorIs(t, err, boom)

	_, err = embedfit.Fit("alpha", "", "", zeroTokenizer{}, policy(4, 0, 0, embedconfig.TruncationReject))
	require.Error(t, err)

	_, err = embedfit.Fit("alpha", "too long ", "", words{}, policy(1, 0, 0, embedconfig.TruncationReject))
	require.ErrorIs(t, err, embedfit.ErrInputTooLong)
}

func TestPolicyFromUsesInputLimits(t *testing.T) {
	limits := embedconfig.InputLimits{
		MaxTokens: 32, OverlapTokens: 4, MaxSpans: 3, Truncation: embedconfig.TruncationReject,
	}
	assert.Equal(t, embedfit.Policy{
		MaxTokens: 32, OverlapTokens: 4, MaxSpans: 3, Truncation: embedconfig.TruncationReject,
	}, embedfit.PolicyFrom(limits))
	_, err := embedfit.Fit("alpha", "", "", words{}, embedfit.Policy{MaxTokens: 4})
	require.Error(t, err)
}

func TestFitCutsAtAParagraphBreakThatStartsBeforeThePreferredWindow(t *testing.T) {
	// The window is 20 runes and its preferred part starts at rune 15. The
	// break occupies runes 14 and 15, so its first newline sits just before
	// that part. A later space must not win over the paragraph break.
	source := strings.Repeat("a", 14) + "\n\n" + "bb cc" + strings.Repeat("x", 20)
	got, err := embedfit.Fit(source, "", "", runes{}, policy(20, 0, 0, embedconfig.TruncationDropTail))
	require.NoError(t, err)
	require.NotEmpty(t, got.Spans)
	assert.Equal(t, strings.Repeat("a", 14), got.Spans[0].Text)
	assert.False(t, got.Spans[0].Truncated)
}

func TestFitAcceptsASeparatorAtTheStartOfThePreferredWindow(t *testing.T) {
	// The fitted window is "abc " and its last quarter is only that space,
	// so the only separator is at index 0. Reject mode must keep the term
	// after the break instead of failing as a hard cut. The span drops the
	// trailing space.
	got, err := embedfit.Fit("abc def", "", "", words{}, policy(1, 0, 0, embedconfig.TruncationReject))
	require.NoError(t, err)
	require.Len(t, got.Spans, 2)
	assert.Equal(t, "abc", got.Spans[0].Text)
	assert.False(t, got.Spans[0].Truncated)
	assert.Equal(t, "def", got.Spans[1].Text)
	assert.False(t, got.Spans[1].Truncated)
	assert.False(t, got.TailDropped)
}

func policy(maxTokens, overlap, maxSpans int, truncation embedconfig.Truncation) embedfit.Policy {
	return embedfit.Policy{
		MaxTokens: maxTokens, OverlapTokens: overlap, MaxSpans: maxSpans, Truncation: truncation,
	}
}

func assertBudget(t *testing.T, got embedfit.Result, prefix, suffix string, tok embedfit.Tokenizer, maxTokens int) {
	t.Helper()
	for _, span := range got.Spans {
		n, err := tok.Count(prefix + span.Text + suffix)
		require.NoError(t, err)
		assert.LessOrEqual(t, n, maxTokens)
	}
}

func covers(source string, got embedfit.Result) bool {
	total := utf8.RuneCountInString(source)
	if len(got.Spans) == 0 || got.Spans[0].RuneStart != 0 || got.Spans[len(got.Spans)-1].RuneEnd != total {
		return false
	}
	cursor := 0
	for _, span := range got.Spans {
		if span.RuneStart > cursor {
			return false
		}
		if span.RuneEnd > cursor {
			cursor = span.RuneEnd
		}
	}
	return cursor == total
}

type words struct{}

func (words) Count(text string) (int, error) {
	if strings.TrimSpace(text) == "" {
		return 0, nil
	}
	return len(strings.Fields(text)), nil
}

type runes struct{}

func (runes) Count(text string) (int, error) {
	return utf8.RuneCountInString(text), nil
}

type failTokenizer struct{ err error }

func (f failTokenizer) Count(string) (int, error) { return 0, f.err }

type zeroTokenizer struct{}

func (zeroTokenizer) Count(string) (int, error) { return 0, nil }
