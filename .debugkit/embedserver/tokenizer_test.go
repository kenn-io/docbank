package embedserver

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestTokenizerMatchesPinnedWordPiece(t *testing.T) {
	path := filepath.Join(t.TempDir(), "tokenizer.json")
	require.NoError(t, os.WriteFile(path, []byte(`{"model":{"vocab":{"[PAD]":0,"[UNK]":100,"[CLS]":101,"[SEP]":102,"[MASK]":103,"cafe":1,"ᄒ":3,"##ᄀ":4,"##ᄋ":5,"हनद":6,"भष":7,"日":8,"本":9,"語":10,"a":15,"##a":16,"b":17,"ab":19,"、":30,"。":31,"$":32,"##©":34,"##b":35,"\ufada":41}},"added_tokens":[{"content":"[PAD]","id":0,"special":true},{"content":"[UNK]","id":100,"special":true},{"content":"[CLS]","id":101,"special":true},{"content":"[SEP]","id":102,"special":true},{"content":"[MASK]","id":103,"special":true}]}`), 0o600))
	tokenizer, err := loadTokenizer(path)
	require.NoError(t, err)
	cases := []struct {
		name, text string
		ids        []int
	}{
		{"accented", "CAFÉ", []int{101, 1, 102}},
		{"Korean first decomposition", "한국어", []int{101, 3, 4, 5, 102}},
		{"Hindi marks", "हिन्दी भाषा", []int{101, 6, 7, 102}},
		{"CJK punctuation", "日本語、a。", []int{101, 8, 9, 10, 30, 15, 31, 102}},
		{"special tokens", "a[CLS][MASK][SEP][UNK][PAD]b", []int{101, 15, 101, 103, 102, 100, 0, 17, 102}},
		{"zero width", "a\u200bb", []int{101, 19, 102}},
		{"BEL", "a\ab", []int{101, 19, 102}},
		{"controls", "a\x00\ufffd\ue000b", []int{101, 19, 102}},
		{"Unicode whitespace", "a\u0085b\u2000a\u3000b", []int{101, 15, 17, 15, 17, 102}},
		{"ASCII symbol", "a$b", []int{101, 15, 32, 17, 102}},
		{"nonASCII symbol", "a©b", []int{101, 15, 34, 35, 102}},
		{"CJK gap", "a\U0002b820b", []int{101, 100, 102}},
		{"CJK boundary", "a\U0002b920b", []int{101, 15, 100, 17, 102}},
		{"unassigned CJK", "a\ufadab", []int{101, 15, 41, 17, 102}},
		{"whole word fallback", "aaa☃", []int{101, 100, 102}},
		{"leading separator", "▁ab", []int{101, 100, 102}},
		{"internal separator", "a▁b", []int{101, 15, 17, 102}},
		{"newer combining mark", "a\u0897b", []int{101, 100, 102}},
		{"fallback after cap", strings.Repeat("a", 2047) + "☃", []int{101, 100, 102}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) { assert.Equal(t, tc.ids, tokenizer.encode(tc.text)) })
	}
	ids := tokenizer.encode("b " + strings.Repeat("a", 3000))
	require.Len(t, ids, 2048)
	assert.Equal(t, []int{101, 17, 15}, ids[:3])
	assert.Equal(t, 102, ids[len(ids)-1])
	for _, id := range ids[3 : len(ids)-1] {
		assert.Equal(t, 16, id)
	}
}
