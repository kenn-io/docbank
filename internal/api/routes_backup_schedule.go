package api

import (
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"net/http"
	"time"

	"github.com/danielgtaylor/huma/v2"

	"go.kenn.io/docbank/internal/store"
)

// BackupSchedule reports runtime overrides beside the schedule they produce.
// Revision is the vault settings revision shared with photo settings.
type BackupSchedule struct {
	IntervalSeconds          *int64 `json:"interval_seconds,omitzero" doc:"Runtime override; absent inherits config.toml."`
	KeepLast                 *int   `json:"keep_last,omitzero" doc:"Runtime override; absent inherits config.toml."`
	EffectiveIntervalSeconds int64  `json:"effective_interval_seconds" doc:"Interval in force; 0 disables scheduling."`
	EffectiveKeepLast        int    `json:"effective_keep_last" doc:"Scheduled points retention keeps."`
	Revision                 int64  `json:"revision" minimum:"1"`
}

type SetBackupScheduleRequest struct {
	IntervalSeconds *int64 `json:"interval_seconds,omitzero" nullable:"false" minimum:"0" maximum:"9223372036" doc:"Omit to keep the override; 0 disables scheduling."`
	ResetInterval   bool   `json:"reset_interval,omitzero" doc:"Inherit the interval from config.toml."`
	KeepLast        *int   `json:"keep_last,omitzero" nullable:"false" minimum:"1" doc:"Omit to keep the override."`
	ResetKeepLast   bool   `json:"reset_keep_last,omitzero" doc:"Inherit keep_last from config.toml."`
	null            bool
}

// UnmarshalJSON refuses null so a client cannot mistake it for a reset.
func (in *SetBackupScheduleRequest) UnmarshalJSON(raw []byte) error {
	type fields SetBackupScheduleRequest
	var decoded fields
	if err := json.Unmarshal(raw, &decoded); err != nil {
		return err
	}
	var members map[string]jsontext.Value
	if err := json.Unmarshal(raw, &members); err != nil {
		return err
	}
	*in = SetBackupScheduleRequest(decoded)
	in.null = string(members["interval_seconds"]) == "null" || string(members["keep_last"]) == "null"
	return nil
}

type backupScheduleOutput struct {
	ETag string `header:"ETag"`
	Body BackupSchedule
}

func registerBackupScheduleRoutes(api huma.API, d Deps, g *gate) {
	// A schedule started from a settings change captures through this gate.
	d.Gate = g
	huma.Register(api, huma.Operation{
		OperationID: "getBackupSchedule", Method: http.MethodGet,
		Path: "/api/v1/backup/schedule", Summary: "Inspect the backup schedule and its runtime overrides",
	}, func(ctx context.Context, _ *struct{}) (*backupScheduleOutput, error) {
		settings, err := d.Store.PhotoSettings(ctx)
		if err != nil {
			return nil, FromStoreError(err)
		}
		return backupScheduleResponse(d, settings), nil
	})

	huma.Register(api, huma.Operation{
		OperationID: "setBackupSchedule", Method: http.MethodPut,
		Path: "/api/v1/backup/schedule", Summary: "Override the backup interval or retention without a restart",
	}, func(ctx context.Context, in *struct {
		IfMatch string `header:"If-Match"`
		Body    SetBackupScheduleRequest
	}) (*backupScheduleOutput, error) {
		revision, err := parseIfMatch(in.IfMatch)
		if err != nil {
			return nil, err
		}
		body := in.Body
		switch {
		case body.null:
			return nil, huma.Error422UnprocessableEntity("Use reset_interval or reset_keep_last to inherit config.toml")
		case body.IntervalSeconds == nil && body.KeepLast == nil && !body.ResetInterval && !body.ResetKeepLast:
			return nil, huma.Error422UnprocessableEntity("Set or reset interval_seconds or keep_last")
		}
		update := store.PhotoSettingsUpdate{
			BackupIntervalSeconds: body.IntervalSeconds, ResetBackupInterval: body.ResetInterval,
			BackupKeepLast: body.KeepLast, ResetBackupKeepLast: body.ResetKeepLast,
			ValidateSchedule: func(next store.PhotoSettings) error {
				if err := effectiveBackupConfig(d.Cfg.Backup, next).ValidateSchedule(); err != nil {
					return NewError(http.StatusUnprocessableEntity, "validation", err.Error())
				}
				return nil
			},
		}
		var out *backupScheduleOutput
		err = g.mutate(ctx, func() error {
			settings, err := d.Store.SetPhotoSettings(ctx, revision, update)
			if err != nil {
				if problem, ok := errors.AsType[*Error](err); ok {
					return problem
				}
				return FromStoreError(err)
			}
			select {
			case d.BackupScheduleWake <- struct{}{}:
			default:
			}
			if effectiveBackupConfig(d.Cfg.Backup, settings).Interval.Std() > 0 {
				if err := startBackupSchedule(d); err != nil {
					return err
				}
			}
			out = backupScheduleResponse(d, settings)
			return nil
		})
		return out, err
	})
}

func backupScheduleResponse(d Deps, settings store.PhotoSettings) *backupScheduleOutput {
	cfg := effectiveBackupConfig(d.Cfg.Backup, settings)
	return &backupScheduleOutput{ETag: revisionETag(settings.Revision), Body: BackupSchedule{
		IntervalSeconds: settings.BackupIntervalSeconds, KeepLast: settings.BackupKeepLast,
		EffectiveIntervalSeconds: int64(cfg.Interval.Std() / time.Second), EffectiveKeepLast: cfg.KeepLast,
		Revision: settings.Revision,
	}}
}
