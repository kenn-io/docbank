package main

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/docbank/internal/api"
)

func TestBackupProgressRendererPlain(t *testing.T) {
	var out bytes.Buffer
	renderer := newBackupProgressRenderer(&out, backupProgressPlain)
	renderer.interval = 0
	renderer.handle(api.BackupProgress{Stage: "attachments", Done: 1, Total: 2, BytesDone: 2048})
	renderer.handle(api.BackupProgress{
		Stage: "attachments", Done: 2, Total: 2,
		BytesDone: 4096, BytesTotal: 4096, Final: true,
	})
	renderer.finish()

	text := out.String()
	assert.NotContains(t, text, "\r")
	assert.Equal(t, 2, strings.Count(text, "\n"))
	assert.Contains(t, text, "attachments: 1/2")
	assert.Contains(t, text, "2.0 KiB")
	assert.Contains(t, text, "100%")
	assert.Contains(t, text, "(done)")
}

func TestBackupProgressRendererBarRedrawsAndTerminates(t *testing.T) {
	var out bytes.Buffer
	renderer := newBackupProgressRenderer(&out, backupProgressBar)
	defer renderer.finish()
	renderer.interval = 0
	renderer.handle(api.BackupProgress{Stage: "metadata", Done: 0, Total: 1})
	assert.True(t, strings.HasPrefix(out.String(), "\r"))
	assert.False(t, strings.HasSuffix(out.String(), "\n"))
	assert.Contains(t, out.String(), "░")

	out.Reset()
	renderer.handle(api.BackupProgress{Stage: "metadata", Done: 1, Total: 1, Final: true})
	assert.True(t, strings.HasPrefix(out.String(), "\r"))
	assert.True(t, strings.HasSuffix(out.String(), "\n"))
	assert.Contains(t, out.String(), strings.Repeat("█", backupProgressBarWidth))
}

func TestBackupProgressRendererKeepsSilentBarAlive(t *testing.T) {
	var out bytes.Buffer
	renderer := newBackupProgressRenderer(&out, backupProgressBar)
	renderer.interval = 5 * time.Millisecond
	renderer.tick = time.Millisecond
	defer renderer.finish()
	renderer.handle(api.BackupProgress{Stage: "metadata", Total: 1})
	renderer.mu.Lock()
	renderer.stageStart = time.Now().Add(-10 * time.Second)
	out.Reset()
	renderer.mu.Unlock()

	require.Eventually(t, func() bool {
		renderer.mu.Lock()
		defer renderer.mu.Unlock()
		return strings.Contains(out.String(), "10s")
	}, time.Second, time.Millisecond)
}

func TestBackupProgressRendererFinishClosesOpenBar(t *testing.T) {
	var out bytes.Buffer
	renderer := newBackupProgressRenderer(&out, backupProgressBar)
	renderer.handle(api.BackupProgress{Stage: "freeze", Total: 1})
	out.Reset()
	renderer.finish()
	assert.Equal(t, "\n", out.String())
	renderer.finish()
	assert.Equal(t, "\n", out.String(), "finish is idempotent")
}

func TestBackupProgressModesAndClamping(t *testing.T) {
	for value, want := range map[string]backupProgressMode{
		"": backupProgressAuto, "auto": backupProgressAuto,
		"bar": backupProgressBar, "plain": backupProgressPlain,
	} {
		got, err := backupProgressModeFromFlag(value)
		require.NoError(t, err)
		assert.Equal(t, want, got)
	}
	_, err := backupProgressModeFromFlag("noisy")
	require.Error(t, err)
	assert.InDelta(t, 0, backupProgressPercent(-1, 10), 0)
	assert.InDelta(t, 0, backupProgressPercent(1, 0), 0)
	assert.InDelta(t, 100, backupProgressPercent(12, 10), 0)
}
