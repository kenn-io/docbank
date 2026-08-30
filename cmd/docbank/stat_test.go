package main

import (
	"bytes"
	"context"
	"encoding/json/v2"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/client"
)

func TestStatCLIInspectsLiveAndTrashedNodes(t *testing.T) {
	_ = setupVaultHome(t)
	source := writeSourceFile(t, "record.txt", "inspectable content")
	_, err := runCLI(t, "add", source, "--dest", "/archive")
	require.NoError(t, err)

	c, err := client.Ensure(context.Background())
	require.NoError(t, err)
	node, err := c.Stat(context.Background(), "/archive/record.txt")
	require.NoError(t, err)
	selector := formatNodeSelector(node.ID)

	out, err := runCLI(t, "stat", "/archive/record.txt")
	require.NoError(t, err, out)
	assert.Contains(t, out, "selector:  "+selector)
	assert.Contains(t, out, "state:     live")
	assert.Contains(t, out, `path:      "/archive/record.txt"`)
	assert.Contains(t, out, "kind:      file")
	assert.Contains(t, out, "version:   "+node.CurrentVersionID)
	assert.Contains(t, out, "sha256:    "+node.BlobHash)
	assert.Contains(t, out, "md5:       "+node.MD5)
	assert.Contains(t, out, "mime:      \"text/plain; charset=utf-8\"")

	out, err = runCLI(t, "stat", selector, "--json")
	require.NoError(t, err, out)
	var got api.Node
	require.NoError(t, json.Unmarshal([]byte(out), &got))
	// The metadata worker may publish between the first stat and this read.
	node.SourceMetadata = got.SourceMetadata
	assert.Equal(t, node, got)

	_, err = runCLI(t, "rm", selector)
	require.NoError(t, err)
	out, err = runCLI(t, "stat", selector)
	require.NoError(t, err, out)
	assert.Contains(t, out, "state:     trashed")
	assert.NotContains(t, out, "path:")
	assert.Contains(t, out, "trashed:")

	out, err = runCLI(t, "stat", selector, "--json")
	require.NoError(t, err, out)
	got = api.Node{}
	require.NoError(t, json.Unmarshal([]byte(out), &got))
	assert.NotEmpty(t, got.TrashedAt)
	assert.Empty(t, got.Path)
}

func TestStatCLIShowsEmbeddedSourceMetadata(t *testing.T) {
	_ = setupVaultHome(t)
	source := writeSourceFile(t, "report.pdf", statMetadataPDF())
	_, err := runCLI(t, "add", source, "--dest", "/archive")
	require.NoError(t, err)
	var out string
	require.Eventually(t, func() bool {
		out, err = runCLI(t, "stat", "/archive/report.pdf")
		return err == nil && strings.Contains(out, `"Synthetic report"`)
	}, 5*time.Second, 25*time.Millisecond)
	assert.Contains(t, out, "metadata:      source-metadata/v1")
	assert.Contains(t, out, "page_count:  2")
}

func statMetadataPDF() string {
	objects := []string{
		"<< /Type /Catalog /Pages 2 0 R >>",
		"<< /Type /Pages /Kids [3 0 R 4 0 R] /Count 2 >>",
		"<< /Type /Page /Parent 2 0 R /MediaBox [0 0 612 792] >>",
		"<< /Type /Page /Parent 2 0 R /MediaBox [0 0 612 792] >>",
		"<< /Title (Synthetic report) /Author (Ada; Grace) >>",
	}
	var output bytes.Buffer
	_, _ = output.WriteString("%PDF-1.4\n")
	offsets := make([]int, len(objects))
	for index, object := range objects {
		offsets[index] = output.Len()
		_, _ = fmt.Fprintf(&output, "%d 0 obj\n%s\nendobj\n", index+1, object)
	}
	xref := output.Len()
	_, _ = fmt.Fprintf(&output, "xref\n0 %d\n0000000000 65535 f \n", len(objects)+1)
	for _, offset := range offsets {
		_, _ = fmt.Fprintf(&output, "%010d 00000 n \n", offset)
	}
	_, _ = fmt.Fprintf(&output,
		"trailer\n<< /Size %d /Root 1 0 R /Info 5 0 R >>\nstartxref\n%d\n%%%%EOF\n", len(objects)+1, xref)
	return output.String()
}

func TestStatCLIValidatesSelectorBeforeDaemonStartup(t *testing.T) {
	t.Setenv("DOCBANK_HOME", t.TempDir())
	_, err := runCLI(t, "stat", "relative/path")
	require.ErrorContains(t, err, "absolute virtual path")
}
