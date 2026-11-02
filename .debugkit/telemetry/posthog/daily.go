package posthog

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"os"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/gofrs/flock"
	"go.kenn.io/kit/atomicfile"
	"golang.org/x/sync/semaphore"
)

// Status describes queue acceptance, confirmed delivery, daily deduplication, or disabled telemetry.
type Status string

const (
	// StatusDelivered means PostHog accepted the matching event.
	StatusDelivered Status = "delivered"
	StatusQueued    Status = "queued"
	StatusSkipped   Status = "skipped"
	StatusDisabled  Status = "disabled"
)

// DailyClaims persists installation-scoped UTC daily claims at a caller-owned path.
type DailyClaims struct {
	sem     *semaphore.Weighted
	path    string
	owner   string
	pending *dailyReservation
	write   func(string, []byte, ...atomicfile.Option) error
}

type dailyReservation struct {
	key, day, marker string
	accepted         bool
}

const dailyClaimLimit = 35

// NewDailyClaims uses path and its adjacent lock file; its parent must exist.
func NewDailyClaims(path string) *DailyClaims {
	return &DailyClaims{path: path, owner: rand.Text(), sem: semaphore.NewWeighted(1)}
}

type dailyEvent struct {
	key    string
	claims *DailyClaims
}

// WithDailyEvent counts each filtered key once per installation and UTC day.
func WithDailyEvent(event, keyProperty string, claims *DailyClaims) Option {
	return postHogOptionFunc(func(c *postHogReporterConfig) {
		if c.dailyEvents == nil {
			c.dailyEvents = make(map[string]dailyEvent)
		}
		c.dailyEvents[strings.TrimSpace(event)] = dailyEvent{strings.TrimSpace(keyProperty), claims}
	})
}

type dailyState struct {
	Version int                 `json:"version"`
	Days    map[string][]string `json:"days"`
}

func (s *dailyState) compact() bool {
	// Four-part keys mark unresolved reservations; released readers preserve them.
	markers := make(map[string]struct{ claim, day string })
	unresolved := make(map[string]map[string]bool)
	for marker, days := range s.Days {
		var parts []string
		if json.Unmarshal([]byte(marker), &parts) != nil || len(parts) != 4 || len(days) != 1 {
			continue
		}
		encoded, _ := json.Marshal(parts[:3]) //nolint:errchkjson // strings always encode as JSON
		claim := string(encoded)
		markers[marker] = struct{ claim, day string }{claim, days[0]}
		if unresolved[claim] == nil {
			unresolved[claim] = make(map[string]bool)
		}
		unresolved[claim][days[0]] = true
	}
	changed := false
	for key, days := range s.Days {
		if _, marker := markers[key]; marker {
			continue
		}
		start, accepted := 0, 0
		for i, day := range slices.Backward(days) {
			if !unresolved[key][day] {
				accepted++
				if accepted == dailyClaimLimit {
					start = i
					break
				}
			}
		}
		changed = changed || start > 0 || len(days) == 0
		if len(days) == 0 {
			delete(s.Days, key)
		} else {
			s.Days[key] = days[start:]
		}
	}
	for marker, reservation := range markers {
		if !slices.Contains(s.Days[reservation.claim], reservation.day) {
			delete(s.Days, marker)
			changed = true
		}
	}
	return changed
}

func newerDailyVersion(data []byte) (json.Number, bool) {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	var number json.Number
	if token, err := decoder.Token(); err == nil && token == json.Delim('{') {
		for decoder.More() {
			key, err := decoder.Token()
			if err != nil {
				break
			}
			var value json.RawMessage
			if err := decoder.Decode(&value); err != nil {
				break
			}
			// Like encoding/json, a non-numeric duplicate leaves an earlier number in place.
			if !strings.EqualFold(key.(string), "version") {
				continue
			}
			if literal, ok := jsonNumber(value); ok {
				number = literal
			}
		}
	}
	if number != "" && number != "1" {
		version, parseErr := strconv.ParseFloat(string(number), 64)
		// Refuse unsupported numeric literals that may be at least 1; other shapes are damage.
		atLeastOne := parseErr == nil && version >= 1
		overflowed := errors.Is(parseErr, strconv.ErrRange) && math.IsInf(version, 1)
		return number, atLeastOne || overflowed
	}
	return "", false
}

func jsonNumber(value json.RawMessage) (json.Number, bool) {
	decoder := json.NewDecoder(bytes.NewReader(value))
	decoder.UseNumber()
	var decoded any
	if err := decoder.Decode(&decoded); err != nil {
		return "", false
	}
	number, ok := decoded.(json.Number)
	return number, ok
}

func (d *DailyClaims) report(ctx context.Context, logger *slog.Logger, identity, event, key string, now func() time.Time, send func(time.Time) (Status, error)) (Status, error) {
	if logger == nil {
		logger = slog.Default()
	}
	if err := d.sem.Acquire(ctx, 1); err != nil {
		return "", fmt.Errorf("lock daily telemetry: %w", err)
	}
	defer d.sem.Release(1)

	lock := flock.New(d.path + ".lock")
	_, err := lock.TryLockContext(ctx, 10*time.Millisecond)
	if err != nil {
		return "", fmt.Errorf("lock daily telemetry: %w", err)
	}
	defer func() { _ = lock.Unlock() }()
	var state dailyState
	data, err := os.ReadFile(d.path)
	if err == nil {
		if number, newer := newerDailyVersion(data); newer {
			return "", fmt.Errorf("unsupported daily telemetry version %s", number)
		}
		valid := json.Unmarshal(data, &state) == nil && state.Version == 1 && state.Days != nil
	validateDates:
		for _, days := range state.Days {
			for _, day := range days {
				if _, err := time.Parse(time.DateOnly, day); err != nil {
					valid = false
					break validateDates
				}
			}
		}
		if !valid {
			state = dailyState{}
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", fmt.Errorf("read daily telemetry: %w", err)
	}
	if state.Days == nil {
		state = dailyState{Version: 1, Days: make(map[string][]string)}
	}
	compacted := state.compact()

	save := func() error {
		data, err := json.Marshal(state)
		if err != nil {
			return err
		}
		write := d.write
		if write == nil {
			write = atomicfile.WriteFile
		}
		return write(d.path, data, atomicfile.WithPrivate())
	}
	settle := func() error {
		reservation := d.pending
		if !slices.Contains(state.Days[reservation.marker], reservation.day) {
			d.pending = nil
			return nil
		}
		delete(state.Days, reservation.marker)
		if !reservation.accepted {
			state.Days[reservation.key] = slices.DeleteFunc(state.Days[reservation.key], func(day string) bool { return day == reservation.day })
		}
		state.compact()
		err := save()
		if err == nil || errors.Is(err, atomicfile.ErrPublished) {
			d.pending = nil
			compacted = false
		}
		return err
	}
	if d.pending != nil {
		if err := settle(); err != nil {
			return "", fmt.Errorf("settle daily telemetry: %w", err)
		}
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	encoded, _ := json.Marshal([]string{identity, event, key}) //nolint:errchkjson // strings always encode as JSON
	claim := string(encoded)
	timestamp := now().UTC()
	day := timestamp.Format(time.DateOnly)
	if slices.Contains(state.Days[claim], day) {
		if compacted {
			if err := save(); err != nil {
				logger.WarnContext(ctx, "compact daily telemetry failed", "error", err)
			}
		}
		return StatusSkipped, nil
	}
	marker, _ := json.Marshal([]string{identity, event, key, d.owner}) //nolint:errchkjson // strings always encode as JSON
	d.pending = &dailyReservation{key: claim, day: day, marker: string(marker)}
	state.Days[claim] = append(state.Days[claim], day)
	state.Days[string(marker)] = []string{day}
	if err := save(); err != nil {
		if errors.Is(err, atomicfile.ErrPublished) {
			releaseErr := settle()
			if d.pending != nil {
				return "", errors.Join(
					fmt.Errorf("reserve daily telemetry: %w", err),
					fmt.Errorf("release daily telemetry: %w", releaseErr),
				)
			}
		} else {
			d.pending = nil
		}
		return "", fmt.Errorf("reserve daily telemetry: %w", err)
	}
	status, captureErr := send(timestamp)
	if captureErr == nil && status == StatusQueued {
		d.pending.accepted = true
		if err := settle(); err != nil {
			logger.WarnContext(ctx, "accept daily telemetry failed", "error", err)
		}
		return status, nil
	}
	if err := settle(); err != nil {
		return "", errors.Join(captureErr, fmt.Errorf("release daily telemetry: %w", err))
	}
	return status, captureErr
}
