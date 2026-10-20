package backupapp

import (
	"cmp"
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

type scheduledPoint struct {
	id string
	at time.Time
}

func scheduledPoints(manifests []*backup.Manifest) ([]scheduledPoint, error) {
	var points []scheduledPoint
	for _, manifest := range manifests {
		if manifest.Options.Tag != ScheduledTag {
			continue
		}
		at, err := time.Parse(time.RFC3339Nano, manifest.CreatedAt)
		if err != nil {
			return nil, fmt.Errorf("scheduled snapshot %s timestamp: %w", manifest.SnapshotID, err)
		}
		points = append(points, scheduledPoint{manifest.SnapshotID, at})
	}
	slices.SortFunc(points, func(a, b scheduledPoint) int {
		if order := b.at.Compare(a.at); order != 0 {
			return order
		}
		return cmp.Compare(b.id, a.id)
	})
	return points, nil
}

// ScheduleDelay derives the next capture and stale warning from durable manifests.
// A future timestamp requires a recovery capture after a clock correction.
func ScheduleDelay(manifests []*backup.Manifest, now time.Time, interval time.Duration) (time.Duration, bool, error) {
	points, err := scheduledPoints(manifests)
	if err != nil || len(points) == 0 {
		return 0, false, err
	}
	age := now.Sub(points[0].at)
	// Compare halves to avoid overflow when doubling a configured duration.
	stale := age > 48*time.Hour && age > interval && age-interval > interval
	if age < 0 || age >= interval {
		return 0, stale, nil
	}
	return interval - age, stale, nil
}

// ScheduledRetention selects old scheduled points, always preserving the new capture.
func ScheduledRetention(manifests []*backup.Manifest, keep int, created string) ([]string, error) {
	if keep <= 0 {
		return nil, errors.New("scheduled backup keep_last must be positive")
	}
	points, err := scheduledPoints(manifests)
	if err != nil {
		return nil, err
	}
	remaining := keep - 1
	var remove []string
	for _, point := range points {
		if point.id == created {
			continue
		}
		if remaining > 0 {
			remaining--
		} else {
			remove = append(remove, point.id)
		}
	}
	return remove, nil
}
