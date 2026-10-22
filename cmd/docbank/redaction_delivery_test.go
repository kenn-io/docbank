package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/internal/processing"
)

func TestRedactionLocalPreflight(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("DOCBANK_HOME", filepath.Join(dir, "absent-vault"))
	bad := filepath.Join(dir, "bad.json")
	require.NoError(t, os.WriteFile(bad, []byte(`{"unknown":true}`), 0600))
	for _, cmd := range []*cobra.Command{newRedactionResolveCommand(), newRedactionRenderCommand()} {
		args := []string{bad, bad}
		if cmd.Name() == "render" {
			args = append(args, bad, filepath.Join(dir, "out.zip"))
		}
		cmd.SetArgs(args)
		cmd.SetOut(io.Discard)
		cmd.SetErr(io.Discard)
		err := cmd.ExecuteContext(t.Context())
		require.Equal(t, exitUsage, commandExitCode(err, true))
		_, err = os.Stat(filepath.Join(dir, "absent-vault"))
		require.ErrorIs(t, err, os.ErrNotExist)
	}
}

type redactionFailedOutput struct{ err error }

func (w redactionFailedOutput) Write([]byte) (int, error) { return 0, w.err }

func TestRedactionPublication(t *testing.T) {
	for _, scenario := range []string{"concurrent destination", "canceled", "output failure"} {
		t.Run(scenario, func(t *testing.T) {
			dir := t.TempDir()
			destination := filepath.Join(dir, "out.zip")
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			sentinel := errors.New("synthetic output failure")
			cmd := &cobra.Command{}
			cmd.SetContext(ctx)
			cmd.SetOut(&bytes.Buffer{})
			if scenario == "output failure" {
				cmd.SetOut(redactionFailedOutput{sentinel})
			}
			err := publishRedaction(cmd, destination, false, false, func(_ context.Context, file *os.File) (processing.RedactionArchive, error) {
				_, err := file.WriteString("already verified archive")
				require.NoError(t, err)
				if scenario == "concurrent destination" {
					require.NoError(t, os.WriteFile(destination, []byte("concurrent"), 0600))
				}
				if scenario == "canceled" {
					cancel()
				}
				return processing.RedactionArchive{Size: 24}, nil
			})
			require.Error(t, err)
			data, readErr := os.ReadFile(destination)
			switch scenario {
			case "concurrent destination":
				require.NoError(t, readErr)
				require.Equal(t, "concurrent", string(data))
			case "canceled":
				require.ErrorIs(t, err, context.Canceled)
				require.ErrorIs(t, readErr, os.ErrNotExist)
			case "output failure":
				require.ErrorIs(t, err, sentinel)
				require.Contains(t, err.Error(), "already saved")
				require.NoError(t, readErr)
				require.Equal(t, "already verified archive", string(data))
			}
			files, err := filepath.Glob(filepath.Join(dir, "docbank-redaction-*"))
			require.NoError(t, err)
			require.Empty(t, files)
		})
	}
}
