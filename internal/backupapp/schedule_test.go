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
		{"due", 24 * time.Hour, 24 * time.Hour, 0, false},
		{"recent", 20 * time.Minute, time.Hour, 40 * time.Minute, false},
		{"twice the interval", 2 * time.Hour, time.Hour, 0, false},
		{"short interval stale", 3 * time.Hour, time.Hour, 0, true},
		{"future clock", -24 * time.Hour, time.Hour, time.Hour, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			points := []*backup.Manifest{{SnapshotID: "scheduled", CreatedAt: now.Add(-tc.age).Format(time.RFC3339), Options: backup.ManifestOptions{Tag: ScheduledTag("vault-a")}}}
			delay, stale := ScheduleDelay(points, ScheduledTag("vault-a"), now, tc.interval)
			require.Equal(t, tc.delay, delay)
			require.Equal(t, tc.stale, stale)
		})
	}
}

func TestScheduledRetentionPreservesManualAndNewestPoints(t *testing.T) {
	// ListSnapshots returns validated history in chronological order.
	points := []*backup.Manifest{
		{SnapshotID: "manual", CreatedAt: "2025-01-01T00:00:00Z", Options: backup.ManifestOptions{Tag: "docbank:scheduled"}},
		{SnapshotID: "foreign", CreatedAt: "2025-02-01T00:00:00Z", Options: backup.ManifestOptions{Tag: ScheduledTag("vault-b")}},
		{SnapshotID: "old", CreatedAt: "2026-01-01T00:00:00Z", Options: backup.ManifestOptions{Tag: ScheduledTag("vault-a")}},
		{SnapshotID: "created", CreatedAt: "2026-02-01T00:00:00Z", Options: backup.ManifestOptions{Tag: ScheduledTag("vault-a")}},
		{SnapshotID: "future", CreatedAt: "2027-01-01T00:00:00Z", Options: backup.ManifestOptions{Tag: ScheduledTag("vault-a")}},
	}
	remove := ScheduledRetention(points, ScheduledTag("vault-a"), 2)
	require.Equal(t, []string{"old"}, remove)
	remove = ScheduledRetention(points, ScheduledTag("vault-a"), 1)
	require.Equal(t, []string{"created", "old"}, remove)
}
