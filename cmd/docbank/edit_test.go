package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json/v2"
	"fmt"
	"net"
	"os"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/apiclient"
	"go.kenn.io/docbank/internal/daemonconn"
	"go.kenn.io/docbank/internal/packagetest"
)

func TestEditCreatesVersionAndSkipsUnchangedContent(t *testing.T) {
	_ = setupVaultHome(t)
	initial := writeSourceFile(t, "notes.txt", "initial text")
	_, err := runCLI(t, "add", initial, "--dest", "/inbox")
	require.NoError(t, err)
	c, err := daemonconn.Ensure(t.Context())
	require.NoError(t, err)
	initialNode, err := c.API().ResolvePath(t.Context(), &apiclient.ResolvePathRequestOptions{Query: &apiclient.ResolvePathQuery{Path: "/inbox/notes.txt"}})

	require.NoError(t, err)
	initialVersion := initialNode.CurrentVersionID

	setEditHelper(t, "edited text")
	out, err := runCLI(t, "edit", formatNodeSelector(initialNode.ID), "--progress", "plain")
	require.NoError(t, err, out)
	assert.Contains(t, out, "download:")
	assert.Contains(t, out, "hash:")
	assert.Contains(t, out, "upload:")
	assert.Contains(t, out, "updated /inbox/notes.txt to version")

	out, err = runCLI(t, "cat", "/inbox/notes.txt")
	require.NoError(t, err)
	assert.Equal(t, "edited text", out)
	out, err = runCLI(t, "versions", "cat", initialVersion)
	require.NoError(t, err)
	assert.Equal(t, "initial text", out)
	out, err = runCLI(t, "versions", "list", "/inbox/notes.txt", "--json")
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
	out, err = runCLI(t, "versions", "list", "/inbox/notes.txt", "--json")
	require.NoError(t, err)
	require.NoError(t, json.Unmarshal([]byte(out), &versions))
	assert.Equal(t, 2, versions.Total)

	setEditHelper(t, "edited text")
	out, err = runCLI(t, "edit", "/inbox/notes.txt", "--mime-type", "application/x-notes",
		"--progress", "plain")
	require.NoError(t, err, out)
	assert.Contains(t, out, "updated /inbox/notes.txt to version")
	out, err = runCLI(t, "versions", "list", "/inbox/notes.txt", "--json")
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

func TestEditAuthorityMismatchIsIntegrityFailure(t *testing.T) {
	node := api.Node{BlobHash: strings.Repeat("a", 64), Size: 12}
	stream := &daemonconn.ContentStream{BlobHash: strings.Repeat("b", 64), Size: node.Size}
	err := validateEditStreamAuthority(stream, node)
	require.Error(t, err)
	assert.Equal(t, exitIntegrity, commandExitCode(err, true))

	stream.BlobHash = node.BlobHash
	require.NoError(t, validateEditStreamAuthority(stream, node))
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

	setEditHelper(t, "stale editor output")
	editor := holdEditHelper(t)
	done := make(chan editResult, 1)
	go func() {
		got, runErr := runCLI(t, "edit", "/inbox/notes.txt", "--progress", "plain")
		done <- editResult{got, runErr}
	}()
	release := editor.started(t, done)

	c, err := daemonconn.Ensure(t.Context())
	require.NoError(t, err)
	node, err := c.API().ResolvePath(t.Context(), &apiclient.ResolvePathRequestOptions{Query: &apiclient.ResolvePathQuery{Path: "/inbox/notes.txt"}})

	require.NoError(t, err)
	concurrent := []byte("concurrent replacement")
	sum := sha256.Sum256(concurrent)
	_, err = c.ReplaceContent(t.Context(), node.ID, node.Revision, node.MimeType,
		hex.EncodeToString(sum[:]), int64(len(concurrent)), bytes.NewReader(concurrent))
	require.NoError(t, err)
	release()

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

	setEditHelper(t, "initial text")
	editor := holdEditHelper(t)
	done := make(chan editResult, 1)
	go func() {
		got, runErr := runCLI(t, "edit", "/inbox/notes.txt", "--progress", "plain")
		done <- editResult{got, runErr}
	}()
	release := editor.started(t, done)

	c, err := daemonconn.Ensure(t.Context())
	require.NoError(t, err)
	node, err := c.API().ResolvePath(t.Context(), &apiclient.ResolvePathRequestOptions{Query: &apiclient.ResolvePathQuery{Path: "/inbox/notes.txt"}})

	require.NoError(t, err)
	concurrent := []byte("concurrent replacement")
	sum := sha256.Sum256(concurrent)
	_, err = c.ReplaceContent(t.Context(), node.ID, node.Revision, node.MimeType,
		hex.EncodeToString(sum[:]), int64(len(concurrent)), bytes.NewReader(concurrent))
	require.NoError(t, err)
	release()

	finished := <-done
	require.ErrorContains(t, finished.err, "revision mismatch", finished.out)
	assert.NotContains(t, finished.out, "unchanged /inbox/notes.txt")
	out, err := runCLI(t, "cat", "/inbox/notes.txt")
	require.NoError(t, err)
	assert.Equal(t, string(concurrent), out)
}

func TestMain(m *testing.M) {
	if len(os.Args) > 1 && (os.Args[1] == "internal-email-pdf-worker" || os.Args[1] == "internal-pdfstamp-worker") {
		os.Exit(runProcess(os.Args[1:], os.Stdout, os.Stderr))
	}
	if os.Getenv("DOCBANK_EDIT_TEST_HELPER") == "1" {
		os.Exit(runEditHelper())
	}
	code := packagetest.RunWithLockRegistry(m)
	if docbankBuildDir != "" {
		if err := os.RemoveAll(docbankBuildDir); err != nil {
			fmt.Fprintln(os.Stderr, err)
		}
	}
	os.Exit(code)
}

func runEditHelper() int {
	path := os.Args[len(os.Args)-1]
	if address := os.Getenv("DOCBANK_EDIT_TEST_CONTROL"); address != "" {
		// Connecting tells the test the editor is open. The editor then waits
		// until the test writes a line or closes the connection.
		connection, err := (&net.Dialer{}).DialContext(context.Background(), "tcp", address)
		if err != nil {
			return 21
		}
		_, err = bufio.NewReader(connection).ReadString('\n')
		_ = connection.Close()
		if err != nil {
			return 22
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
	t.Setenv("DOCBANK_EDIT_TEST_CONTROL", "")
	t.Setenv("VISUAL", `"`+os.Args[0]+`"`)
	t.Setenv("EDITOR", "")
}

// editHelperControl holds the editor helper open until the test releases it.
type editHelperControl struct {
	connections chan net.Conn
}

// holdEditHelper makes the next editor helper connect to the test and wait.
func holdEditHelper(t *testing.T) editHelperControl {
	t.Helper()
	listener, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:0")
	require.NoError(t, err)
	t.Cleanup(func() { _ = listener.Close() })
	t.Setenv("DOCBANK_EDIT_TEST_CONTROL", listener.Addr().String())
	control := editHelperControl{connections: make(chan net.Conn, 1)}
	go func() {
		connection, acceptErr := listener.Accept()
		if acceptErr == nil {
			control.connections <- connection
		}
	}()
	return control
}

// started waits until the editor helper is open and returns a function that
// lets it finish. done reports an edit that ended before the editor opened.
func (control editHelperControl) started(t *testing.T, done <-chan editResult) func() {
	t.Helper()
	select {
	case connection := <-control.connections:
		t.Cleanup(func() { _ = connection.Close() })
		return func() {
			_, err := connection.Write([]byte("continue\n"))
			require.NoError(t, err)
			require.NoError(t, connection.Close())
		}
	case finished := <-done:
		require.FailNow(t, "edit ended before the editor opened", "%v: %s", finished.err, finished.out)
		return nil
	}
}

type editResult struct {
	out string
	err error
}
