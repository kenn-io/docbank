package main

import (
	"bytes"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestPartyCommandsExposeJSONAndFields(t *testing.T) {
	t.Parallel()
	for _, args := range [][]string{{"list", "--help"}, {"show", "--help"}, {"create", "--help"}, {"edit", "--help"}, {"retire", "--help"}} {
		cmd := newPartyCommand()
		var out bytes.Buffer
		cmd.SetOut(&out)
		cmd.SetArgs(args)
		require.NoError(t, cmd.Execute())
		assert.Contains(t, out.String(), "--json")
	}
}

func TestPartyExplicitRevisionMustBePositive(t *testing.T) {
	t.Parallel()
	for _, args := range [][]string{{"edit", "id", "--name", "Example", "--revision=0"}, {"retire", "id", "--revision=0"}, {"retire", "id", "--revision=-1"}} {
		cmd := newPartyCommand()
		cmd.SetArgs(args)
		require.ErrorContains(t, cmd.Execute(), "revision must be positive")
	}
}
