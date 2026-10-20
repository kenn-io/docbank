package backupapp

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kit/backup"
)

func TestScheduleDelay(t *testing.T) {
	now := time.Date(2026, 1, 5, 12, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name                 string
		age, interval, delay time.Duration
		stale                bool
	}{
		{"recent", time.Hour, 24 * time.Hour, 23 * time.Hour, false},
		{"due", 24 * time.Hour, 24 * time.Hour, 0, false},
		{"stale after restart", 49 * time.Hour, 24 * time.Hour, 0, true},
		{"long interval", 49 * time.Hour, 48 * time.Hour, 0, false},
		{"future clock", -24 * time.Hour, time.Hour, time.Hour, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			points := []*backup.Manifest{{SnapshotID: "scheduled", CreatedAt: now.Add(-tc.age).Format(time.RFC3339), Options: backup.ManifestOptions{Tag: ScheduledTag}}}
			delay, stale, err := ScheduleDelay(points, "", now, tc.interval)
			require.NoError(t, err)
			require.Equal(t, tc.delay, delay)
			require.Equal(t, tc.stale, stale)
		})
	}
	delay, stale, err := ScheduleDelay(nil, "", now, time.Hour)
	require.NoError(t, err)
	require.Zero(t, delay)
	require.False(t, stale)
}

func TestScheduledRetentionPreservesManualAndNewCapture(t *testing.T) {
	// ListSnapshots returns validated history in chronological order.
	points := []*backup.Manifest{
		{SnapshotID: "manual", CreatedAt: "2025-01-01T00:00:00Z"},
		{SnapshotID: "old", CreatedAt: "2026-01-01T00:00:00Z", Options: backup.ManifestOptions{Tag: ScheduledTag}},
		{SnapshotID: "created", CreatedAt: "2026-02-01T00:00:00Z", Options: backup.ManifestOptions{Tag: ScheduledTag}},
		{SnapshotID: "future", CreatedAt: "2027-01-01T00:00:00Z", Options: backup.ManifestOptions{Tag: ScheduledTag}},
	}
	remove := ScheduledRetention(points, "", 2, "created")
	require.Equal(t, []string{"old"}, remove)
	remove = ScheduledRetention(points, "", 1, "created")
	require.Equal(t, []string{"future", "old"}, remove)
}

func TestScheduledRetentionWithoutNewCapturePreservesHistory(t *testing.T) {
	points := []*backup.Manifest{
		{SnapshotID: "older", Options: backup.ManifestOptions{Tag: ScheduledTag}},
		{SnapshotID: "manual"},
		{SnapshotID: "old", Options: backup.ManifestOptions{Tag: ScheduledTag}},
	}
	remove := ScheduledRetention(points, "", 1, "removed-new-capture")
	require.Empty(t, remove)
}
