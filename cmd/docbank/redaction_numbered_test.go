package main

import (
	"encoding/json/v2"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestNumberedRedactionCLIAdmission(t *testing.T) {
	home := filepath.Join(t.TempDir(), "not-a-vault")
	require.NoError(t, os.WriteFile(home, []byte("synthetic"), 0600))
	t.Setenv("DOCBANK_HOME", home)
	_, err := runCLI(t, "redaction", "reserve", "prep.json", "masks.json", "review.json",
		"--namespace", "11111111-1111-4111-8111-111111111111", "--start", "1")
	require.ErrorContains(t, err, "--retain-original")
	_, err = runCLI(t, "bates", "export", "download", "11111111-1111-4111-8111-111111111111",
		filepath.Join(t.TempDir(), "numbered.pdf"), "--review", "review.json")
	require.ErrorContains(t, err, "--preparation, --masks and --review together")
}

func TestNumberedVerificationReportsVersionMismatch(t *testing.T) {
	f := newRedactionVerifyFixture(t)
	for _, changed := range []string{"renderer", "writer"} {
		t.Run(changed, func(t *testing.T) {
			prep, review := f.request.Preparation, f.review
			if changed == "renderer" {
				prep.Evidence.RendererSHA256 = strings.Repeat("a", 64)
			} else {
				review.Recipe.WriterVersion = "recorded-writer"
			}
			// Reach the actual local-file admission without a daemon.
			dir := t.TempDir()
			pp, mp, rp := filepath.Join(dir, "prep.json"), filepath.Join(dir, "masks.json"), filepath.Join(dir, "review.json")
			for path, value := range map[string]any{pp: prep, mp: f.request.Masks, rp: review} {
				data, err := json.Marshal(value)
				require.NoError(t, err)
				require.NoError(t, os.WriteFile(path, data, 0600))
			}
			_, err := runCLI(t, "bates", "export", "download", "11111111-1111-4111-8111-111111111111", filepath.Join(dir, "out.pdf"),
				"--preparation", pp, "--masks", mp, "--review", rp)
			require.ErrorContains(t, err, "verifier version differs")
			require.Equal(t, exitGeneral, commandExitCode(err, true))
		})
	}
}
