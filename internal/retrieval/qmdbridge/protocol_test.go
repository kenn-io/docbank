package qmdbridge

import (
	"math"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestDecodeResponseAcceptsExplicitEmptyAndNumericZero(t *testing.T) {
	items, err := decodeResponse([]byte(`{"results":[]}`), 1)
	require.NoError(t, err)
	assert.NotNil(t, items)
	items, err = decodeResponse([]byte(`{"results":[{"docid":"#one","file":"qmd://synthetic/documents/a.md","title":"x","score":0,"context":null,"snippet":"x"}]}`), 1)
	require.NoError(t, err)
	require.Len(t, items, 1)
	require.NotNil(t, items[0].Score)
	assert.Zero(t, *items[0].Score)
	assert.Nil(t, items[0].Context)
}

func TestDecodeResponseRejectsStructuralAmbiguityAndInvalidScores(t *testing.T) {
	tests := map[string]string{
		"omitted results":  `{}`,
		"null results":     `{"results":null}`,
		"missing score":    `{"results":[{"docid":"#one","file":"qmd://synthetic/documents/a.md","title":"x","context":null,"snippet":"x"}]}`,
		"null score":       `{"results":[{"docid":"#one","file":"qmd://synthetic/documents/a.md","title":"x","score":null,"context":null,"snippet":"x"}]}`,
		"unknown member":   `{"results":[],"extra":true}`,
		"duplicate member": `{"results":[],"results":[]}`,
		"trailing":         `{"results":[]} true`,
		"score above one":  `{"results":[{"docid":"#one","file":"qmd://synthetic/documents/a.md","title":"x","score":1.1,"context":null,"snippet":"x"}]}`,
	}
	for name, body := range tests {
		t.Run(name, func(t *testing.T) {
			_, err := decodeResponse([]byte(body), 10)
			require.ErrorIs(t, err, ErrInvalidResponse)
		})
	}
	invalidUTF8 := append([]byte(`{"results":[],"`), 0xff)
	invalidUTF8 = append(invalidUTF8, []byte(`":true}`)...)
	_, err := decodeResponse(invalidUTF8, 10)
	require.ErrorIs(t, err, ErrInvalidResponse)
}

func TestDecodeResponseBoundsCandidatesBeforeAppend(t *testing.T) {
	item := `{"docid":"#one","file":"qmd://synthetic/documents/a.md","title":"x","score":0.5,"context":null,"snippet":"x"}`
	items, err := decodeResponse([]byte(`{"results":[`+item+`]}`), 1)
	require.NoError(t, err)
	require.Len(t, items, 1)
	_, err = decodeResponse([]byte(`{"results":[`+item+`,`+item+`]}`), 1)
	require.ErrorIs(t, err, ErrInvalidResponse)
}

func TestWireResultValidationBoundsMetadataAndFiniteScore(t *testing.T) {
	contextText := "ok"
	score := 0.5
	base := wireResult{DocID: "#one", File: "qmd://synthetic/documents/a.md", Title: "x", Score: &score, Context: &contextText, Snippet: "x"}
	assert.True(t, validWireResult(base, 1))
	for name, mutate := range map[string]func(*wireResult){
		"docid":   func(v *wireResult) { v.DocID = strings.Repeat("x", maximumTokenBytes+1) },
		"file":    func(v *wireResult) { v.File = strings.Repeat("x", maximumFileBytes+1) },
		"title":   func(v *wireResult) { v.Title = strings.Repeat("x", maximumTitleBytes+1) },
		"context": func(v *wireResult) { x := strings.Repeat("x", maximumContextBytes+1); v.Context = &x },
		"snippet": func(v *wireResult) { v.Snippet = "xx" },
		"nan":     func(v *wireResult) { value := math.NaN(); v.Score = &value },
	} {
		t.Run(name, func(t *testing.T) {
			value := base
			mutate(&value)
			assert.False(t, validWireResult(value, 1), "%+v", value)
		})
	}
}
