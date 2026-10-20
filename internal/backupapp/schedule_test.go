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
		empty, otherTags     bool
	}{
		{"recent", time.Hour, 24 * time.Hour, 23 * time.Hour, false, false, false},
		{"due", 24 * time.Hour, 24 * time.Hour, 0, false, false, false},
		{"stale after restart", 49 * time.Hour, 24 * time.Hour, 0, true, false, false},
		{"long interval", 49 * time.Hour, 48 * time.Hour, 0, false, false, false},
		{"future clock", -24 * time.Hour, time.Hour, time.Hour, false, false, false},
		{"empty", 0, time.Hour, 0, false, true, false},
		{"manual and foreign tags", time.Hour, time.Hour, 0, false, false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			points := []*backup.Manifest{{SnapshotID: "scheduled", CreatedAt: now.Add(-tc.age).Format(time.RFC3339), Options: backup.ManifestOptions{Tag: ScheduledTag("vault-a")}}}
			if tc.empty {
				points = nil
			}
			if tc.otherTags {
				points = append(points,
					&backup.Manifest{SnapshotID: "manual", CreatedAt: now.Format(time.RFC3339), Options: backup.ManifestOptions{Tag: "docbank:scheduled"}},
					&backup.Manifest{SnapshotID: "foreign", CreatedAt: now.Add(time.Hour).Format(time.RFC3339), Options: backup.ManifestOptions{Tag: ScheduledTag("vault-b")}},
				)
			}
			delay, stale, err := ScheduleDelay(points, ScheduledTag("vault-a"), now, tc.interval)
			require.NoError(t, err)
			require.Equal(t, tc.delay, delay)
			require.Equal(t, tc.stale, stale)
		})
	}
}

func TestScheduledRetentionPreservesManualAndNewCapture(t *testing.T) {
	// ListSnapshots returns validated history in chronological order.
	points := []*backup.Manifest{
		{SnapshotID: "manual", CreatedAt: "2025-01-01T00:00:00Z", Options: backup.ManifestOptions{Tag: "docbank:scheduled"}},
		{SnapshotID: "foreign", CreatedAt: "2025-02-01T00:00:00Z", Options: backup.ManifestOptions{Tag: ScheduledTag("vault-b")}},
		{SnapshotID: "old", CreatedAt: "2026-01-01T00:00:00Z", Options: backup.ManifestOptions{Tag: ScheduledTag("vault-a")}},
		{SnapshotID: "created", CreatedAt: "2026-02-01T00:00:00Z", Options: backup.ManifestOptions{Tag: ScheduledTag("vault-a")}},
		{SnapshotID: "future", CreatedAt: "2027-01-01T00:00:00Z", Options: backup.ManifestOptions{Tag: ScheduledTag("vault-a")}},
	}
	remove := ScheduledRetention(points, ScheduledTag("vault-a"), 2, "created")
	require.Equal(t, []string{"old"}, remove)
	remove = ScheduledRetention(points, ScheduledTag("vault-a"), 1, "created")
	require.Equal(t, []string{"future", "old"}, remove)
	remove = ScheduledRetention(points, ScheduledTag("vault-a"), 1, "removed-new-capture")
	require.Empty(t, remove)
}
