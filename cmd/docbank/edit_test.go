package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/client"
)

func TestEditCreatesVersionAndSkipsUnchangedContent(t *testing.T) {
	_ = setupVaultHome(t)
	initial := writeSourceFile(t, "notes.txt", "initial text")
	_, err := runCLI(t, "add", initial, "--dest", "/inbox")
	require.NoError(t, err)
	c, err := client.Ensure(t.Context())
	require.NoError(t, err)
	initialNode, err := c.Stat(t.Context(), "/inbox/notes.txt")
	require.NoError(t, err)
	initialVersion := initialNode.CurrentVersionID

	setEditHelper(t, "edited text")
	out, err := runCLI(t, "edit", "/inbox/notes.txt", "--progress", "plain")
	require.NoError(t, err, out)
	assert.Contains(t, out, "download:")
	assert.Contains(t, out, "hash:")
	assert.Contains(t, out, "upload:")
	assert.Contains(t, out, "updated /inbox/notes.txt to version")

	out, err = runCLI(t, "cat", "/inbox/notes.txt")
	require.NoError(t, err)
	assert.Equal(t, "edited text", out)
	out, err = runCLI(t, "version", initialVersion, "--content")
	require.NoError(t, err)
	assert.Equal(t, "initial text", out)
	out, err = runCLI(t, "versions", "/inbox/notes.txt", "--json")
	require.NoError(t, err)
	var versions api.ContentVersionPage
	require.NoError(t, json.Unmarshal([]byte(out), &versions))
	require.Len(t, versions.Items, 2)
	assert.Equal(t, "content_replace", versions.Items[0].TransitionKind)
	assert.Equal(t, initialNode.MimeType, versions.Items[0].MimeType)

	setEditHelper(t, "edited text")
	out, err = runCLI(t, "edit", "/inbox/notes.txt", "--progress", "plain")
	require.NoError(t, err, out)
	assert.Contains(t, out, "unchanged /inbox/notes.txt (version ")
	assert.NotContains(t, out, "upload:")
	out, err = runCLI(t, "versions", "/inbox/notes.txt", "--json")
	require.NoError(t, err)
	require.NoError(t, json.Unmarshal([]byte(out), &versions))
	assert.Equal(t, 2, versions.Total)

	setEditHelper(t, "edited text")
	out, err = runCLI(t, "edit", "/inbox/notes.txt", "--mime-type", "application/x-notes",
		"--progress", "plain")
	require.NoError(t, err, out)
	assert.Contains(t, out, "updated /inbox/notes.txt to version")
	out, err = runCLI(t, "versions", "/inbox/notes.txt", "--json")
	require.NoError(t, err)
	require.NoError(t, json.Unmarshal([]byte(out), &versions))
	assert.Equal(t, 3, versions.Total)
	assert.Equal(t, "application/x-notes", versions.Items[0].MimeType)
}

func TestEditorCommandPrecedenceAndParsing(t *testing.T) {
	t.Setenv("VISUAL", "code --wait")
	t.Setenv("EDITOR", "nano")
	command, err := editorCommand("")
	require.NoError(t, err)
	assert.Equal(t, []string{"code", "--wait"}, command)

	command, err = editorCommand(`"custom editor" --block`)
	require.NoError(t, err)
	assert.Equal(t, []string{"custom editor", "--block"}, command)

	t.Setenv("VISUAL", "")
	command, err = editorCommand("")
	require.NoError(t, err)
	assert.Equal(t, []string{"nano"}, command)
}

func TestEditStagePatternUsesOnlySafeBoundedExtensions(t *testing.T) {
	tests := map[string]string{
		"notes.md":                    "document-*.md",
		"notes.tar.gz":                "document-*.gz",
		"notes.*":                     "document-*",
		"notes.bad:stream":            "document-*",
		"notes.question?":             "document-*",
		"notes.é":                     "document-*",
		"notes.abcdefghijklmnop":      "document-*.abcdefghijklmnop",
		"notes.abcdefghijklmnopq":     "document-*",
		"notes.non-ascii-extension-é": "document-*",
	}
	for name, expected := range tests {
		t.Run(name, func(t *testing.T) {
			assert.Equal(t, expected, editStagePattern(name))
		})
	}
}

func TestEditFailureAndConcurrentReplacementDoNotOverwrite(t *testing.T) {
	_ = setupVaultHome(t)
	initial := writeSourceFile(t, "notes.txt", "initial text")
	_, err := runCLI(t, "add", initial, "--dest", "/inbox")
	require.NoError(t, err)

	setEditHelper(t, "editor output")
	t.Setenv("DOCBANK_EDIT_TEST_FAIL", "1")
	_, err = runCLI(t, "edit", "/inbox/notes.txt", "--progress", "plain")
	require.ErrorContains(t, err, "editor")
	out, err := runCLI(t, "cat", "/inbox/notes.txt")
	require.NoError(t, err)
	assert.Equal(t, "initial text", out)
	t.Setenv("DOCBANK_EDIT_TEST_FAIL", "")

	marker := t.TempDir() + string(os.PathSeparator) + "started"
	release := t.TempDir() + string(os.PathSeparator) + "release"
	setEditHelper(t, "stale editor output")
	t.Setenv("DOCBANK_EDIT_TEST_MARKER", marker)
	t.Setenv("DOCBANK_EDIT_TEST_RELEASE", release)
	type result struct {
		out string
		err error
	}
	done := make(chan result, 1)
	go func() {
		got, runErr := runCLI(t, "edit", "/inbox/notes.txt", "--progress", "plain")
		done <- result{got, runErr}
	}()
	require.Eventually(t, func() bool {
		_, statErr := os.Stat(marker)
		return statErr == nil
	}, 10*time.Second, 20*time.Millisecond)

	c, err := client.Ensure(t.Context())
	require.NoError(t, err)
	node, err := c.Stat(t.Context(), "/inbox/notes.txt")
	require.NoError(t, err)
	concurrent := []byte("concurrent replacement")
	sum := sha256.Sum256(concurrent)
	_, err = c.ReplaceContent(t.Context(), node.ID, node.Revision, node.MimeType,
		hex.EncodeToString(sum[:]), int64(len(concurrent)), bytes.NewReader(concurrent))
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(release, []byte("continue"), 0o600))

	finished := <-done
	require.ErrorContains(t, finished.err, "revision mismatch", finished.out)
	out, err = runCLI(t, "cat", "/inbox/notes.txt")
	require.NoError(t, err)
	assert.Equal(t, string(concurrent), out)
}

func TestEditUnchangedRejectsConcurrentReplacement(t *testing.T) {
	_ = setupVaultHome(t)
	initial := writeSourceFile(t, "notes.txt", "initial text")
	_, err := runCLI(t, "add", initial, "--dest", "/inbox")
	require.NoError(t, err)

	marker := t.TempDir() + string(os.PathSeparator) + "started"
	release := t.TempDir() + string(os.PathSeparator) + "release"
	setEditHelper(t, "initial text")
	t.Setenv("DOCBANK_EDIT_TEST_MARKER", marker)
	t.Setenv("DOCBANK_EDIT_TEST_RELEASE", release)
	type result struct {
		out string
		err error
	}
	done := make(chan result, 1)
	go func() {
		got, runErr := runCLI(t, "edit", "/inbox/notes.txt", "--progress", "plain")
		done <- result{got, runErr}
	}()
	require.Eventually(t, func() bool {
		_, statErr := os.Stat(marker)
		return statErr == nil
	}, 10*time.Second, 20*time.Millisecond)

	c, err := client.Ensure(t.Context())
	require.NoError(t, err)
	node, err := c.Stat(t.Context(), "/inbox/notes.txt")
	require.NoError(t, err)
	concurrent := []byte("concurrent replacement")
	sum := sha256.Sum256(concurrent)
	_, err = c.ReplaceContent(t.Context(), node.ID, node.Revision, node.MimeType,
		hex.EncodeToString(sum[:]), int64(len(concurrent)), bytes.NewReader(concurrent))
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(release, []byte("continue"), 0o600))

	finished := <-done
	require.ErrorContains(t, finished.err, "revision mismatch", finished.out)
	assert.NotContains(t, finished.out, "unchanged /inbox/notes.txt")
	out, err := runCLI(t, "cat", "/inbox/notes.txt")
	require.NoError(t, err)
	assert.Equal(t, string(concurrent), out)
}

func TestMain(m *testing.M) {
	if os.Getenv("DOCBANK_EDIT_TEST_HELPER") == "1" {
		os.Exit(runEditHelper())
	}
	os.Exit(m.Run())
}

func runEditHelper() int {
	path := os.Args[len(os.Args)-1]
	if marker := os.Getenv("DOCBANK_EDIT_TEST_MARKER"); marker != "" {
		if err := os.WriteFile(marker, []byte("started"), 0o600); err != nil {
			return 21
		}
	}
	if release := os.Getenv("DOCBANK_EDIT_TEST_RELEASE"); release != "" {
		deadline := time.Now().Add(10 * time.Second)
		for {
			if _, err := os.Stat(release); err == nil {
				break
			}
			if time.Now().After(deadline) {
				return 22
			}
			time.Sleep(20 * time.Millisecond)
		}
	}
	if os.Getenv("DOCBANK_EDIT_TEST_FAIL") == "1" {
		return 23
	}
	if err := os.WriteFile(path, []byte(os.Getenv("DOCBANK_EDIT_TEST_CONTENT")), 0o600); err != nil {
		return 24
	}
	return 0
}

func setEditHelper(t *testing.T, content string) {
	t.Helper()
	t.Setenv("DOCBANK_EDIT_TEST_HELPER", "1")
	t.Setenv("DOCBANK_EDIT_TEST_CONTENT", content)
	t.Setenv("DOCBANK_EDIT_TEST_FAIL", "")
	t.Setenv("DOCBANK_EDIT_TEST_MARKER", "")
	t.Setenv("DOCBANK_EDIT_TEST_RELEASE", "")
	t.Setenv("VISUAL", `"`+os.Args[0]+`"`)
	t.Setenv("EDITOR", "")
}
