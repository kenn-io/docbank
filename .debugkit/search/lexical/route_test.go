package lexical_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kit/search/lexical"
)

func TestCJKOffKeepsEveryQueryOnTheOrdinaryIndex(t *testing.T) {
	var off lexical.CJK
	assert.False(t, off.Enabled())
	for _, text := range []string{"running", "搜索", "かな", "검색", "run 搜索", "42", "、"} {
		assert.Equal(t, lexical.IndexOrdinary, off.IndexFor(text), text)
		_, ok := off.Analyzer(text)
		assert.False(t, ok, text)
	}
}

func TestCharacterPhraseRoutesByScript(t *testing.T) {
	cjk := lexical.EnableCharacterPhrase()
	assert.True(t, cjk.Enabled())
	assert.Equal(t, lexical.IndexOrdinary, cjk.IndexFor("running"))
	assert.Equal(t, lexical.IndexOrdinary, cjk.IndexFor("42"))

	for _, text := range []string{"搜索", "かな", "검색", "run 搜索"} {
		assert.Equal(t, lexical.IndexCJK, cjk.IndexFor(text), text)
		analyzer, ok := cjk.Analyzer(text)
		require.True(t, ok, text)
		assert.Equal(t, lexical.KindCharacterPhrase, analyzer.Identity.Kind)
	}

	prepared, err := mustAnalyzer(t, cjk, "run 搜索").PrepareLiteral("run 搜索")
	require.NoError(t, err)
	assert.Equal(t, `"run" "搜 索"`, prepared.Match)
}

func TestChineseRoutesHanAndLeavesKanaUnsegmented(t *testing.T) {
	_, err := lexical.EnableChinese("", nil)
	require.Error(t, err)

	var calls int
	cjk, err := lexical.EnableChinese("dict-a", func(text string) ([]lexical.Token, error) {
		calls++
		return []lexical.Token{{Text: text}}, nil
	})
	require.NoError(t, err)

	assert.Equal(t, lexical.IndexOrdinary, cjk.IndexFor("running"))

	han, ok := cjk.Analyzer("搜索")
	require.True(t, ok)
	prepared, err := han.PrepareLiteral("搜索")
	require.NoError(t, err)
	assert.Equal(t, lexical.KindChinese, prepared.Identity.Kind)
	assert.Equal(t, 1, calls)

	kana, ok := cjk.Analyzer("かな")
	require.True(t, ok)
	prepared, err = kana.PrepareLiteral("かな")
	require.NoError(t, err)
	assert.Equal(t, `"かな"`, prepared.Match)
	assert.Equal(t, 1, calls)
}

func mustAnalyzer(t *testing.T, cjk lexical.CJK, text string) lexical.Analyzer {
	t.Helper()
	analyzer, ok := cjk.Analyzer(text)
	require.True(t, ok)
	return analyzer
}
