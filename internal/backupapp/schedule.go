package backupapp

import (
	"errors"
	"fmt"
	"slices"
	"time"

	"go.kenn.io/kit/backup"
)

// ScheduledTag identifies recovery points owned by the daemon's retention policy.
const ScheduledTag = "docbank:scheduled"

// ValidateManualTag prevents manual recovery points from entering scheduled retention.
func ValidateManualTag(tag string) error {
	if tag == ScheduledTag {
		return fmt.Errorf("backup tag %q is reserved for scheduled backups", tag)
	}
	return nil
}

// Kit lists validated manifests oldest first; retention consumes newest first.
func scheduledPoints(manifests []*backup.Manifest) []*backup.Manifest {
	var points []*backup.Manifest
	for _, manifest := range slices.Backward(manifests) {
		if manifest.Options.Tag != ScheduledTag {
			continue
		}
		points = append(points, manifest)
	}
	return points
}

// ScheduleDelay derives the next capture and stale warning from durable manifests.
// Future timestamps preserve the same durable deadline across daemon restarts.
func ScheduleDelay(manifests []*backup.Manifest, now time.Time, interval time.Duration) (time.Duration, bool, error) {
	points := scheduledPoints(manifests)
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
	return at.Add(interval).Sub(now), stale, nil
}

// ScheduledRetention selects old scheduled points, always preserving the new capture.
func ScheduledRetention(manifests []*backup.Manifest, keep int, created string) ([]string, error) {
	if keep <= 0 {
		return nil, errors.New("scheduled backup keep_last must be positive")
	}
	points := scheduledPoints(manifests)
	found := false
	for _, point := range points {
		if point.SnapshotID == created {
			found = true
			break
		}
	}
	if !found {
		return nil, nil
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
	return remove, nil
}
