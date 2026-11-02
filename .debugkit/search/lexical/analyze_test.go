package lexical_test

import (
	"database/sql"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kit/search/lexical"
	_ "modernc.org/sqlite"
)

func TestLiteralKeepsIdentifiersPunctuationAndAccents(t *testing.T) {
	a := lexical.Literal()
	got, err := a.PrepareLiteral(`get_views error-401 status:500 café`)
	require.NoError(t, err)
	assert.Equal(t, `"get_views" "error-401" "status:500" "café"`, got.Match)
	assert.Equal(t, "get_views", got.SnippetTerm)
	assert.Equal(t, lexical.KindLiteral, got.Identity.Kind)

	phrase, err := a.PreparePhrase(`fix bug`)
	require.NoError(t, err)
	assert.Equal(t, `"fix bug"`, phrase.Match)

	advanced, err := a.PrepareAdvanced(`"中文" OR "国法"`)
	require.NoError(t, err)
	assert.Equal(t, `"中文" OR "国法"`, advanced.Match)

	indexed, err := a.IndexText("café get_views")
	require.NoError(t, err)
	assert.Equal(t, "café get_views", indexed)

	_, err = a.PrepareLiteral("   ")
	require.Error(t, err)
	_, err = a.PrepareAdvanced("  ")
	require.Error(t, err)
}

func TestCharacterPhrasePreservesAdjacency(t *testing.T) {
	a := lexical.CharacterPhrase()
	literal, err := a.PrepareLiteral("かな カタカナ")
	require.NoError(t, err)
	assert.Equal(t, `"か な" "カ タ カ ナ"`, literal.Match)

	single, err := a.PrepareLiteral("ぬ")
	require.NoError(t, err)
	assert.Equal(t, `"ぬ"`, single.Match)

	hangul, err := a.PrepareLiteral("검색")
	require.NoError(t, err)
	assert.Equal(t, `"검 색"`, hangul.Match)

	mixed, err := a.PrepareLiteral("SQLite 検索します")
	require.NoError(t, err)
	assert.Equal(t, `"SQLite" "検 索 し ま す"`, mixed.Match)

	phrase, err := a.PreparePhrase("かな カタカナ")
	require.NoError(t, err)
	assert.Equal(t, `"か な カ タ カ ナ"`, phrase.Match)

	indexed, err := a.IndexText("SQLiteで検索します。")
	require.NoError(t, err)
	assert.Equal(t, "SQLite で 検 索 し ま す 。", indexed)

	assert.NotEqual(t, lexical.Literal().Identity, a.Identity)
}

func TestChineseSegmentsHanAndNotKana(t *testing.T) {
	var calls int
	segment := func(text string) ([]lexical.Token, error) {
		calls++
		switch text {
		case "法国":
			return []lexical.Token{{Text: "法国"}}, nil
		case "中文搜索":
			return []lexical.Token{{Text: "中文"}, {Text: "搜索"}}, nil
		case "错":
			return []lexical.Token{{Text: "错", Prefix: true}}, nil
		default:
			return []lexical.Token{{Text: "未登录"}}, nil
		}
	}
	runtime, err := lexical.FingerprintRuntime(lexical.ChineseQueryVersion, []lexical.RuntimeFile{
		{Name: "library", Data: []byte("runtime-a")},
		{Name: "jieba.dict.utf8", Data: []byte("dict-a")},
	})
	require.NoError(t, err)
	a, err := lexical.Chinese(runtime, segment)
	require.NoError(t, err)

	han, err := a.PrepareLiteral("法国")
	require.NoError(t, err)
	assert.Equal(t, `"法国"`, han.Match)
	assert.Equal(t, runtime, han.Identity.Runtime)
	assert.Equal(t, 1, calls)

	terms, err := a.PrepareLiteral("中文搜索")
	require.NoError(t, err)
	assert.Equal(t, `"中文" AND "搜索"`, terms.Match)
	assert.Equal(t, "中文", terms.SnippetTerm)

	prefix, err := a.PrepareLiteral("错")
	require.NoError(t, err)
	assert.Equal(t, "错*", prefix.Match)

	kana, err := a.PrepareLiteral("かな")
	require.NoError(t, err)
	assert.Equal(t, `"かな"`, kana.Match)
	assert.Equal(t, 3, calls, "kana must not be segmented")

	mixed, err := a.PrepareLiteral("検索方法を")
	require.NoError(t, err)
	assert.Equal(t, `"検索方法を"`, mixed.Match)
	assert.Equal(t, 3, calls, "kanji with kana keeps adjacency instead of segmentation")

	phrase, err := a.PreparePhrase("中文搜索")
	require.NoError(t, err)
	assert.Equal(t, `"中文搜索"`, phrase.Match)
	assert.Equal(t, 3, calls, "an explicit phrase must not be segmented")

	_, err = a.IndexText("中文")
	require.Error(t, err)
	require.ErrorIs(t, lexical.CheckCompatible(lexical.CharacterPhrase().Identity, a.Identity), lexical.ErrIdentityMismatch)
	require.NoError(t, lexical.CheckCompatible(a.Identity, han.Identity))

	changed, err := lexical.FingerprintRuntime(lexical.ChineseQueryVersion, []lexical.RuntimeFile{
		{Name: "library", Data: []byte("runtime-a")},
		{Name: "jieba.dict.utf8", Data: []byte("dict-b")},
	})
	require.NoError(t, err)
	assert.NotEqual(t, runtime, changed)

	// A zero byte inside a file must not make two different file lists match.
	left, err := lexical.FingerprintRuntime(lexical.ChineseQueryVersion, []lexical.RuntimeFile{
		{Name: "a", Data: []byte("x\x00y")},
		{Name: "b", Data: []byte("z")},
	})
	require.NoError(t, err)
	right, err := lexical.FingerprintRuntime(lexical.ChineseQueryVersion, []lexical.RuntimeFile{
		{Name: "a", Data: []byte("x")},
		{Name: "b\x00y", Data: []byte("z")},
	})
	require.NoError(t, err)
	assert.NotEqual(t, left, right)

	blank, err := lexical.Chinese(runtime, func(string) ([]lexical.Token, error) { return nil, nil })
	require.NoError(t, err)
	_, err = blank.PrepareLiteral("法国")
	require.Error(t, err)
	_, err = lexical.Chinese("", segment)
	require.Error(t, err)
	_, err = lexical.FingerprintRuntime("", []lexical.RuntimeFile{{Name: "library", Data: []byte("x")}})
	require.Error(t, err)
	empty, err := a.PrepareLiteral("没有")
	require.NoError(t, err)
	assert.Equal(t, `"未登录"`, empty.Match)
}

func TestCharacterPhraseMatchesRealFTS(t *testing.T) {
	db, err := sql.Open("sqlite", ":memory:")
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, db.Close()) })
	_, err = db.ExecContext(t.Context(), `
CREATE TABLE docs (id INTEGER PRIMARY KEY, body TEXT);
CREATE VIRTUAL TABLE docs_fts USING fts5(body, tokenize='unicode61');`)
	require.NoError(t, err)

	a := lexical.CharacterPhrase()
	docs := []string{
		"かなを探します。",
		"なかを探します。",
		"カタカナを探します。",
		"カナとカタを探します。",
		"検索方法を説明します。",
		"方法を変えて検索します。",
		"いぬを探します。",
		"ねこを探します。",
		"검색합니다.",
		"색검합니다.",
		"색상입니다.",
		"SQLiteで検索します。",
		"SQLiteで検索し、別の作業をします。",
		"SQLite로 검색합니다.",
		"SQLite 색상 검토입니다.",
		"カタカナとかなを探します。",
		"かな カタカナを探します。",
		"기능을 추가해 검색합니다.",
		"검색합니다.",
		"검색 기능을 추가합니다.",
		"法国的首都是巴黎。",
		"这份材料讨论国法体系。",
		"The runner is running get_views after error-401.",
		"café menu",
	}
	for i, body := range docs {
		indexed, err := a.IndexText(body)
		require.NoError(t, err)
		id := i + 1
		_, err = db.ExecContext(t.Context(), `INSERT INTO docs (id, body) VALUES (?, ?)`, id, body)
		require.NoError(t, err)
		_, err = db.ExecContext(t.Context(), `INSERT INTO docs_fts (rowid, body) VALUES (?, ?)`, id, indexed)
		require.NoError(t, err)
	}

	cases := []struct {
		name   string
		query  string
		phrase bool
		want   []string
		miss   []string
	}{
		{name: "hiragana order", query: "かな", want: []string{"かなを探します。"}, miss: []string{"なかを探します。"}},
		{name: "katakana adjacency", query: "カタカナ", want: []string{"カタカナを探します。"}, miss: []string{"カナとカタを探します。"}},
		{name: "kanji and kana", query: "検索方法を", want: []string{"検索方法を説明します。"}, miss: []string{"方法を変えて検索します。"}},
		{name: "single kana", query: "ぬ", want: []string{"いぬを探します。"}, miss: []string{"ねこを探します。"}},
		{name: "hangul adjacency", query: "검색", want: []string{"검색합니다.", "기능을 추가해 검색합니다.", "SQLite로 검색합니다."}, miss: []string{"색검합니다.", "색상입니다."}},
		{name: "single hangul", query: "검", want: []string{"검색합니다."}, miss: []string{"색상입니다."}},
		{name: "japanese with latin", query: "SQLite 検索します", want: []string{"SQLiteで検索します。"}, miss: []string{"SQLiteで検索し、別の作業をします。"}},
		{name: "korean with latin", query: "SQLite 검색", want: []string{"SQLite로 검색합니다."}, miss: []string{"SQLite 색상 검토입니다."}},
		{name: "separate japanese terms", query: "かな カタカナ", want: []string{"カタカナとかなを探します。", "かな カタカナを探します。"}, miss: []string{"カタカナを探します。"}},
		{name: "separate korean terms", query: "검색 기능", want: []string{"기능을 추가해 검색합니다.", "검색 기능을 추가합니다."}, miss: []string{"검색합니다."}},
		{name: "quoted japanese phrase", query: "かな カタカナ", phrase: true, want: []string{"かな カタカナを探します。"}, miss: []string{"カタカナとかなを探します。"}},
		{name: "quoted korean phrase", query: "검색 기능", phrase: true, want: []string{"검색 기능을 추가합니다."}, miss: []string{"기능을 추가해 검색합니다."}},
		{name: "han adjacency is not segmentation", query: "法国", want: []string{"法国的首都是巴黎。"}, miss: []string{"这份材料讨论国法体系。"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var prepared lexical.Prepared
			var err error
			if tc.phrase {
				prepared, err = a.PreparePhrase(tc.query)
			} else {
				prepared, err = a.PrepareLiteral(tc.query)
			}
			require.NoError(t, err)
			require.NoError(t, lexical.CheckCompatible(a.Identity, prepared.Identity))
			hits := matchBodies(t, db, prepared.Match)
			for _, body := range tc.want {
				assert.Contains(t, hits, body)
			}
			for _, body := range tc.miss {
				assert.NotContains(t, hits, body)
			}
		})
	}
}

func TestLiteralMatchDoesNotParsePunctuation(t *testing.T) {
	db, err := sql.Open("sqlite", ":memory:")
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, db.Close()) })
	_, err = db.ExecContext(t.Context(), `CREATE VIRTUAL TABLE docs_fts USING fts5(body, tokenize='unicode61');
INSERT INTO docs_fts (body) VALUES ('get_views failed with error-401'), ('status 500 elsewhere'), ('café menu');`)
	require.NoError(t, err)
	a := lexical.Literal()
	raw := "status:500"
	err = db.QueryRowContext(t.Context(), `SELECT count(*) FROM docs_fts WHERE docs_fts MATCH ?`, raw).Scan(new(int))
	require.Error(t, err, "unquoted colon is a column filter, not a literal term")
	for _, query := range []string{"error-401", "get_views", "café", "status:500"} {
		prepared, err := a.PrepareLiteral(query)
		require.NoError(t, err)
		var n int
		require.NoError(t, db.QueryRowContext(t.Context(), `SELECT count(*) FROM docs_fts WHERE docs_fts MATCH ?`, prepared.Match).Scan(&n))
		assert.Equal(t, 1, n, query)
	}
}

func matchBodies(t *testing.T, db *sql.DB, match string) []string {
	t.Helper()
	rows, err := db.QueryContext(t.Context(), `SELECT d.body FROM docs_fts JOIN docs AS d ON d.id = docs_fts.rowid WHERE docs_fts MATCH ?`, match)
	require.NoError(t, err)
	defer func() { require.NoError(t, rows.Close()) }()
	var hits []string
	for rows.Next() {
		var body string
		require.NoError(t, rows.Scan(&body))
		hits = append(hits, body)
	}
	require.NoError(t, rows.Err())
	return hits
}
