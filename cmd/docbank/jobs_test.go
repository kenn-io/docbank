package main

import (
	"bytes"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/internal/api"
)

func TestJobsDisplaysLaneControlState(t *testing.T) {
	var out bytes.Buffer
	require.NoError(t, writeJobs(&out, []api.Job{
		{Name: "derive:visual-previews", Status: "running", Controllable: true, Paused: true, Concurrency: 3, CanSetConcurrency: true},
		{Name: "derive:renditions", Status: "running"},
		{Name: "storage:operation", Status: "queued", Kind: "photo_import", Controllable: true},
	}))
	assert.Contains(t, out.String(), "paused limit=3")
	assert.Contains(t, out.String(), "read-only")
	assert.Contains(t, out.String(), "active lane=photo_import")
}

func TestJobsRejectsNonIntegerConcurrencyBeforeConnecting(t *testing.T) {
	for _, value := range []string{"many", "1.5", "999999999999999999999"} {
		cmd := jobsControlCommand("concurrency")
		err := cmd.RunE(cmd, []string{"derive:visual-previews", value})
		require.ErrorContains(t, err, "concurrency must be an integer")
	}
}

func TestJobsPauseResumeAndConcurrency(t *testing.T) {
	t.Setenv("DOCBANK_LOCK_DIR", t.TempDir())
	setupVaultHome(t)
	for _, step := range []struct {
		args []string
		want string
	}{
		{[]string{"pause", "derive:visual-previews"}, "paused=true concurrency=1"},
		{[]string{"concurrency", "derive:visual-previews", "3"}, "paused=true concurrency=3"},
		{[]string{"resume", "derive:visual-previews"}, "paused=false concurrency=3"},
		{[]string{"concurrency", "photo_import", "1"}, "paused=false concurrency=1"},
	} {
		out, err := runCLI(t, append([]string{"jobs"}, step.args...)...)
		require.NoError(t, err)
		assert.Contains(t, out, step.want)
	}
	_, err := runCLI(t, "jobs", "concurrency", "derive:visual-previews", "0")
	require.ErrorContains(t, err, "validation")
}
