package main

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kit/embedclient"
	"go.kenn.io/kit/embedconfig"
	"go.kenn.io/kit/internal/embedassets"
	"go.kenn.io/kit/internal/embedtest"
)

// CI supplies a staged native bundle. This catches missing companion
// libraries and executable-relative lookup failures that model-only parity misses.
func TestBundleNativeParity(t *testing.T) {
	executable, dir := os.Getenv("KIT_EMBED_TEST_EXECUTABLE"), os.Getenv("KIT_EMBED_TEST_DIR")
	if executable == "" || dir == "" {
		t.Skip("native bundle fixture is unset")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Minute)
	defer cancel()
	command := exec.CommandContext(ctx, executable, "--data-dir", dir)
	stderr, err := command.StderrPipe()
	require.NoError(t, err)
	require.NoError(t, command.Start())

	drained := make(chan struct{})
	defer func() { cancel(); _ = command.Wait(); <-drained }()
	scanner := bufio.NewScanner(stderr)
	var endpoint string
	for scanner.Scan() {
		if strings.HasPrefix(scanner.Text(), "http://") {
			endpoint = scanner.Text()
			break
		}
	}
	go func() { _, _ = io.Copy(io.Discard, stderr); close(drained) }()
	require.NotEmpty(t, endpoint, "bundle exited before publishing its endpoint")
	client, err := embedclient.New(embedclient.Options{
		Model:      embedconfig.Model{Name: "nomic-embed-text", Dimensions: 768, Metric: embedconfig.MetricCosine, Normalization: embedconfig.NormalizationNone},
		Deployment: embedconfig.Deployment{BaseURL: endpoint},
	})
	require.NoError(t, err)

	sample := embedtest.Load(t, "../../embedserver/testdata/ollama.json")[0]
	actual, err := client.EncodeFunc(embedconfig.RoleQuery)(ctx, []string{sample.Input})
	require.NoError(t, err)
	require.Len(t, actual, 1)
	sample.AssertParity(t, actual[0])
	if runtime.GOOS == "windows" {
		// Inspect the live process, so installed system copies cannot mask a missing
		// app-local dependency in the bundle under test.
		script := fmt.Sprintf("(Get-Process -Id %d).Modules | ForEach-Object { $_.FileName }", command.Process.Pid)
		output, err := exec.CommandContext(ctx, "powershell.exe", "-NoProfile", "-Command", script).CombinedOutput()
		require.NoError(t, err, string(output))
		for _, name := range embedassets.NativeFiles() {
			assert.Contains(t, strings.ToLower(string(output)), strings.ToLower(filepath.Join(filepath.Dir(executable), name)))
		}
	}
}
