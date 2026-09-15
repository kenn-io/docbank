package client_test

import (
	"encoding/json/v2"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/client"
	"go.kenn.io/docbank/internal/store"
)

func TestExtractProblemFactsCopiesOnlyStableMetadata(t *testing.T) {
	tests := []struct {
		name         string
		code         string
		observed     int
		mapped       error
		wantObserved int
	}{
		{name: "mapped domain error", code: "not_found", mapped: store.ErrNotFound},
		{name: "unknown problem code", code: "synthetic_unknown"},
		{name: "scope overflow", code: "scope_too_large", observed: 4097, wantObserved: 4097},
		{name: "one-byte code", code: "a"},
		{name: "64-byte code", code: "a0_" + strings.Repeat("b", 61)},
	}
	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			err := problemFactsError(t, testCase.code, testCase.observed)
			require.Error(t, err)
			facts, ok := client.ExtractProblemFacts(err)
			require.True(t, ok)
			assert.Equal(t, testCase.code, facts.Code)
			assert.Equal(t, testCase.wantObserved, facts.ObservedScopeCount)
			if testCase.mapped == nil {
				require.NoError(t, facts.MappedError)
			} else {
				require.ErrorIs(t, facts.MappedError, testCase.mapped)
				assert.Same(t, testCase.mapped, facts.MappedError)
			}
			formatted := fmt.Sprintf("%v %+v %#v", facts, facts, facts)
			assert.NotContains(t, formatted, "synthetic-secret")
			assert.NotContains(t, formatted, "/private/vault")
			assert.NotContains(t, formatted, "private-document")
		})
	}

	_, ok := client.ExtractProblemFacts(store.ErrNotFound)
	assert.False(t, ok, "raw sentinel errors are not decoded daemon problem responses")
}

func TestExtractProblemFactsRejectsUnsafeCodes(t *testing.T) {
	tests := []struct {
		name   string
		code   string
		marker string
	}{
		{name: "slash path", code: "a/private/path", marker: "private/path"},
		{name: "spaces", code: "a secret detail", marker: "secret detail"},
		{name: "newline", code: "a\nnewline_secret", marker: "newline_secret"},
		{name: "control", code: "a\x01control_secret", marker: "control_secret"},
		{name: "uppercase", code: "Uppercase_secret", marker: "Uppercase_secret"},
		{name: "non ASCII", code: "aéunicode_secret", marker: "unicode_secret"},
		{name: "empty"},
		{name: "leading digit", code: "1leading_secret", marker: "leading_secret"},
		{name: "over 64 bytes", code: "a" + strings.Repeat("b", 58) + "over64", marker: "over64"},
	}
	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			err := problemFactsError(t, testCase.code, 4097)
			require.Error(t, err)

			facts, ok := client.ExtractProblemFacts(err)
			assert.False(t, ok)
			assert.Equal(t, client.ProblemFacts{}, facts)
			_, codeOK := client.ProblemCode(err)
			assert.False(t, codeOK)
			if testCase.marker != "" {
				formatted := fmt.Sprintf("%v | %+v | %#v", facts, facts, facts)
				assert.NotContains(t, formatted, testCase.marker)
			}
		})
	}
}

func problemFactsError(t *testing.T, code string, observed int) error {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, _ *http.Request) {
		response.Header().Set("Content-Type", "application/problem+json")
		response.WriteHeader(http.StatusUnprocessableEntity)
		err := json.MarshalWrite(response, api.Error{
			Title: "Unprocessable Entity", Status: http.StatusUnprocessableEntity,
			Code: code, Detail: "synthetic-secret /private/vault private-document",
			ObservedScopeCount: observed,
		})
		assert.NoError(t, err)
	}))
	t.Cleanup(server.Close)
	return client.New(server.URL, "synthetic-key").Health(t.Context())
}
