package store

import (
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"

	"go.kenn.io/kit/atomicfile"
	"go.kenn.io/kit/pack"
)

const (
	VisualPreviewLane = "derive:visual-previews"
	// MaxLaneConcurrency bounds lanes whose concurrency is adjustable.
	MaxLaneConcurrency = 4
	laneControlsFile   = "lane-controls.json"
)

var ErrLaneControl = errors.New("invalid lane control")

// ErrLaneControlsFile marks an unreadable or damaged lane-controls.json, as
// opposed to a request for a lane or setting that cannot be controlled.
var ErrLaneControlsFile = errors.New("unreadable lane controls file")

// LaneControl is operational state kept beside the database rather than in
// it, so backups and schema upgrades never carry it.
type LaneControl struct {
	Lane        string `json:"lane"`
	Paused      bool   `json:"paused"`
	Concurrency int    `json:"concurrency"`
	Revision    int64  `json:"revision"`
}

type laneControlSetting struct {
	Paused      *bool          `json:"paused"`
	Concurrency int            `json:"concurrency"`
	Revision    int64          `json:"revision"`
	Unknown     jsontext.Value `json:"-"`
}

var controllableLanes = append([]string{VisualPreviewLane}, storageOperationKinds...)

func ControllableLane(lane string) bool {
	return slices.Contains(controllableLanes, lane)
}

// LaneConcurrencyAdjustable reports whether lane accepts concurrency above 1.
func LaneConcurrencyAdjustable(lane string) bool {
	return lane == VisualPreviewLane
}

func validLaneSetting(lane string, concurrency int) bool {
	limit := 1
	if LaneConcurrencyAdjustable(lane) {
		limit = MaxLaneConcurrency
	}
	return ControllableLane(lane) && concurrency >= 1 && concurrency <= limit
}

// FinishRestoreLaneControls syncs the published reset before recovery marker removal.
func FinishRestoreLaneControls(vaultDir string, replaced bool) error {
	if !replaced {
		return nil
	}
	err := os.Remove(filepath.Join(vaultDir, laneControlsFile))
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("resetting restored lane controls: %w", err)
	}
	if err := pack.SyncDir(vaultDir); err != nil {
		return fmt.Errorf("syncing restored lane controls: %w", err)
	}
	return nil
}

func (s *Store) laneControlsPath() string {
	return filepath.Join(filepath.Dir(s.path), laneControlsFile)
}

func (s *Store) readLaneControls(ctx context.Context) (map[string]laneControlSetting, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	path := s.laneControlsPath()
	settings := make(map[string]laneControlSetting)
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return settings, nil
	}
	if err != nil {
		return nil, fmt.Errorf("reading lane controls: %w: %w", ErrLaneControlsFile, err)
	}
	var entries map[string]jsontext.Value
	if err := json.Unmarshal(data, &entries); err != nil {
		return nil, fmt.Errorf("decoding lane controls %s: %w: %w", path, ErrLaneControlsFile, err)
	}
	if entries == nil {
		return nil, fmt.Errorf("decoding lane controls %s: file holds null: %w",
			path, ErrLaneControlsFile)
	}
	for lane, entry := range entries {
		if !ControllableLane(lane) {
			settings[lane] = laneControlSetting{Unknown: entry}
			continue
		}
		var setting laneControlSetting
		if err := json.Unmarshal(entry, &setting); err != nil {
			return nil, fmt.Errorf("decoding lane controls %s for %q: %w: %w", path, lane, ErrLaneControlsFile, err)
		}
		if setting.Paused == nil || !validLaneSetting(lane, setting.Concurrency) ||
			setting.Revision < 2 {
			return nil, fmt.Errorf("lane controls %s: invalid setting for %q: %w",
				path, lane, ErrLaneControlsFile)
		}
		settings[lane] = setting
	}
	return settings, nil
}

func laneControlFrom(lane string, settings map[string]laneControlSetting) LaneControl {
	setting, found := settings[lane]
	if !found {
		return LaneControl{Lane: lane, Concurrency: 1, Revision: 1}
	}
	return LaneControl{
		Lane: lane, Paused: *setting.Paused,
		Concurrency: setting.Concurrency, Revision: setting.Revision,
	}
}

func (s *Store) LaneControl(ctx context.Context, lane string) (LaneControl, error) {
	if !ControllableLane(lane) {
		return LaneControl{Lane: lane, Concurrency: 1, Revision: 1}, ErrLaneControl
	}
	s.laneControlsMu.Lock()
	defer s.laneControlsMu.Unlock()
	settings, err := s.readLaneControls(ctx)
	if err != nil {
		return LaneControl{}, err
	}
	return laneControlFrom(lane, settings), nil
}

// LaneControls returns the settings of every controllable lane from one read.
func (s *Store) LaneControls(ctx context.Context) (map[string]LaneControl, error) {
	s.laneControlsMu.Lock()
	defer s.laneControlsMu.Unlock()
	settings, err := s.readLaneControls(ctx)
	if err != nil {
		return nil, err
	}
	controls := make(map[string]LaneControl)
	for _, lane := range controllableLanes {
		controls[lane] = laneControlFrom(lane, settings)
	}
	return controls, nil
}

// SetLaneControl replaces a lane's settings when revision is still current.
func (s *Store) SetLaneControl(
	ctx context.Context, control LaneControl, revision int64,
) (LaneControl, error) {
	if !validLaneSetting(control.Lane, control.Concurrency) {
		return LaneControl{}, fmt.Errorf(
			"lane or concurrency cannot be controlled: %w", ErrLaneControl)
	}
	s.laneControlsMu.Lock()
	defer s.laneControlsMu.Unlock()
	settings, err := s.readLaneControls(ctx)
	if err != nil {
		return LaneControl{}, err
	}
	if laneControlFrom(control.Lane, settings).Revision != revision {
		return LaneControl{}, ErrStaleRevision
	}
	control.Revision = revision + 1
	settings[control.Lane] = laneControlSetting{
		Paused: new(control.Paused), Concurrency: control.Concurrency, Revision: control.Revision,
	}
	entries := make(map[string]jsontext.Value, len(settings))
	for lane, setting := range settings {
		if !ControllableLane(lane) {
			entries[lane] = setting.Unknown
			continue
		}
		entry, err := json.Marshal(setting)
		if err != nil {
			return LaneControl{}, fmt.Errorf("encoding lane control %q: %w", lane, err)
		}
		entries[lane] = entry
	}
	data, err := json.Marshal(entries, jsontext.WithIndent("  "))
	if err != nil {
		return LaneControl{}, fmt.Errorf("encoding lane controls: %w", err)
	}
	if err := atomicfile.WriteFile(s.laneControlsPath(), append(data, '\n')); err != nil {
		return LaneControl{}, fmt.Errorf("writing lane controls: %w", err)
	}
	return control, nil
}
