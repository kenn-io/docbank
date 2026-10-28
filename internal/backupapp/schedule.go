package backupapp

import (
	"fmt"
	"slices"
	"time"

	"go.kenn.io/kit/backup"
)

const scheduledTagPrefix = "docbank:scheduled:"

// ScheduledTag identifies recovery points managed by this vault's retention policy.
func ScheduledTag(vaultID string) string {
	return scheduledTagPrefix + vaultID
}

// ErrReservedScheduledTag prevents manual captures from entering scheduled retention.
var ErrReservedScheduledTag = fmt.Errorf("backup tag prefix %q is reserved for scheduled backups", scheduledTagPrefix)

// Kit lists validated manifests oldest first; retention consumes newest first.
func scheduledPoints(manifests []*backup.Manifest, tag string) []*backup.Manifest {
	var points []*backup.Manifest
	for _, manifest := range slices.Backward(manifests) {
		if manifest.Options.Tag != tag {
			continue
		}
		points = append(points, manifest)
	}
	return points
}

// ScheduleDelay derives the next capture and stale warning from durable manifests.
// Future timestamps delay recovery by at most one interval per daemon run.
func ScheduleDelay(manifests []*backup.Manifest, tag string, now time.Time, interval time.Duration) (time.Duration, bool) {
	points := scheduledPoints(manifests, tag)
	if len(points) == 0 {
		return 0, false
	}
	// Kit's ListSnapshots rejects malformed timestamps, so the parse can't fail here.
	at, _ := time.Parse(time.RFC3339, points[0].CreatedAt)
	age := now.Sub(at)
	// Compare halves to avoid overflow when doubling a configured duration.
	stale := age > interval && age-interval > interval
	if age >= interval {
		return 0, stale
	}
	return min(interval, at.Add(interval).Sub(now)), stale
}

// ScheduledRetention selects scheduled points older than the newest keep points.
func ScheduledRetention(manifests []*backup.Manifest, tag string, keep int) []string {
	points := scheduledPoints(manifests, tag)
	var remove []string
	for i, point := range points {
		if i >= keep {
			remove = append(remove, point.SnapshotID)
		}
	}
	return remove
}
