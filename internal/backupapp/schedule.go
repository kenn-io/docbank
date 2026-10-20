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
func ScheduleDelay(manifests []*backup.Manifest, tag string, now time.Time, interval time.Duration) (time.Duration, bool, error) {
	points := scheduledPoints(manifests, tag)
	if len(points) == 0 {
		return 0, false, nil
	}
	at, err := time.Parse(time.RFC3339Nano, points[0].CreatedAt)
	if err != nil {
		return 0, false, fmt.Errorf("scheduled snapshot %s timestamp: %w", points[0].SnapshotID, err)
	}
	age := now.Sub(at)
	// Compare halves to avoid overflow when doubling a configured duration.
	stale := age > 48*time.Hour && age > interval && age-interval > interval
	if age >= interval {
		return 0, stale, nil
	}
	return min(interval, at.Add(interval).Sub(now)), stale, nil
}

// ScheduledRetention selects old scheduled points, always preserving the new capture.
func ScheduledRetention(manifests []*backup.Manifest, tag string, keep int, created string) []string {
	points := scheduledPoints(manifests, tag)
	found := false
	for _, point := range points {
		if point.SnapshotID == created {
			found = true
			break
		}
	}
	if !found {
		return nil
	}
	remaining := keep - 1
	var remove []string
	for _, point := range points {
		if point.SnapshotID == created {
			continue
		}
		if remaining > 0 {
			remaining--
		} else {
			remove = append(remove, point.SnapshotID)
		}
	}
	return remove
}
