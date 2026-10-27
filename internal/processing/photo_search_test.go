package processing

import (
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/internal/query"
)

func TestPhotoSemanticScopeKeepsBarePickFilter(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name, syntax, text string
		meaning, scope     string
		supported          bool
	}{
		{"advanced bare pick", "advanced", "sunset pick", "sunset", "pick", true},
		{"advanced quoted pick", "advanced", `sunset "pick"`, "sunset pick", "", true},
		{"simple pick", "simple", "sunset pick", "sunset pick", "", true},
		{"advanced pick alone", "advanced", "pick", "", "pick", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			meaning, scope, supported := photoSemanticScope(query.Query{V: 1, Mode: "semantic", Syntax: tc.syntax, Text: tc.text})
			require.Equal(t, tc.supported, supported)
			if !supported {
				return
			}
			require.Equal(t, tc.meaning, meaning)
			require.Equal(t, tc.scope, scope.Text)
		})
	}
}
