package main

import (
	"context"
	"crypto/sha256"
	"encoding/json/v2"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document/emailpdf"
	"go.kenn.io/docbank/internal/api"
)

func TestEmailPDFRealDaemonCLI(t *testing.T) {
	binary := os.Getenv("DOCBANK_EMAILPDF_TEST_WORKER")
	bundle := os.Getenv("DOCBANK_EMAILPDF_TEST_BUNDLE")
	fonts := os.Getenv("DOCBANK_EMAILPDF_TEST_FONTS")
	if binary == "" || bundle == "" || fonts == "" {
		t.Skip("requires the explicit pinned real renderer qualification environment")
	}
	bundleHash, err := emailpdf.TreeSHA256(bundle)
	require.NoError(t, err)
	fontsHash, err := emailpdf.TreeSHA256(fonts)
	require.NoError(t, err)
	workspace, err := os.MkdirTemp("", "docbank-emailpdf-cli-") //nolint:usetesting // Retain staging if daemon shutdown cannot be proven; automatic TempDir cleanup would be unsafe.
	require.NoError(t, err)
	vault := filepath.Join(workspace, "vault")
	require.NoError(t, os.Mkdir(vault, 0700))
	invoke := func(ctx context.Context, args ...string) ([]byte, error) {
		cmd := exec.CommandContext(ctx, binary, args...) //nolint:gosec // Explicit qualification binary; arguments are synthetic test inputs.
		cmd.Env = []string{"PATH=/usr/local/bin:/usr/bin:/bin", "LANG=C.UTF-8", "DOCBANK_HOME=" + vault}
		return cmd.Output()
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		_, err := invoke(ctx, "daemon", "stop")
		if !assertNoCLIError(t, err) {
			t.Logf("retained synthetic workspace because daemon stop failed: %s", workspace)
			return
		}
		status, err := invoke(ctx, "daemon", "status", "--json")
		if !assertNoCLIError(t, err) {
			return
		}
		var state struct {
			Running bool `json:"running"`
		}
		require.NoError(t, json.Unmarshal(status, &state))
		require.False(t, state.Running, "refuse cleanup of live synthetic daemon")
		require.NoError(t, os.RemoveAll(workspace))
		_, err = os.Stat(workspace)
		require.ErrorIs(t, err, os.ErrNotExist)
	})
	config := fmt.Sprintf("[email_pdf]\nchromium=%q\nbundle=%q\nbundle_sha256=%q\nversion=%q\nfonts=%q\nfonts_sha256=%q\n", filepath.Join(bundle, "chrome"), bundle, bundleHash, "151.0.7922.34", fonts, fontsHash)
	require.NoError(t, os.WriteFile(filepath.Join(vault, "config.toml"), []byte(config), 0600))
	source := filepath.Join(workspace, "synthetic.eml")
	require.NoError(t, os.WriteFile(source, []byte("Subject: Synthetic CLI proof\r\nDate: Mon, 01 Jan 2024 12:34:56 +0530\r\nContent-Type: text/plain; charset=utf-8\r\n\r\nComplete daemon-first email PDF.\r\n> Full original quote.\r\n"), 0600))
	ctx, cancel := context.WithTimeout(t.Context(), 100*time.Second)
	defer cancel()
	_, err = invoke(ctx, "add", source, "--dest", "/", "--json")
	require.True(t, assertNoCLIError(t, err))
	versions, err := invoke(ctx, "versions", "list", "/synthetic.eml", "--json")
	require.True(t, assertNoCLIError(t, err))
	var page api.ContentVersionPage
	require.NoError(t, json.Unmarshal(versions, &page))
	require.Len(t, page.Items, 1)
	output := filepath.Join(workspace, "message.pdf")
	_, err = invoke(ctx, "email-pdf", page.Items[0].ID, output)
	require.True(t, assertNoCLIError(t, err))
	first, err := os.ReadFile(output)
	require.NoError(t, err)
	pages, err := emailpdf.VerifyPDF(first)
	require.NoError(t, err)
	require.Positive(t, pages)
	_, err = invoke(ctx, "email-pdf", page.Items[0].ID, output, "--overwrite")
	require.True(t, assertNoCLIError(t, err))
	again, err := os.ReadFile(output)
	require.NoError(t, err)
	require.Equal(t, first, again, "same exact daemon recipe must reuse verified bytes")
	t.Logf("real daemon-first CLI PDF pages=%d bytes=%d sha256=%x", pages, len(first), sha256.Sum256(first))
}

func assertNoCLIError(t *testing.T, err error) bool {
	t.Helper()
	if err == nil {
		return true
	}
	exit := &exec.ExitError{}
	if errors.As(err, &exit) {
		t.Errorf("synthetic CLI failed: %v: %s", err, exit.Stderr)
	} else {
		t.Errorf("synthetic CLI failed: %v", err)
	}
	return false
}
