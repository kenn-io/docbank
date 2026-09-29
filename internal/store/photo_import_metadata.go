package store

import (
	"context"
	"database/sql"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
)

type metadataPhotoImportRun struct {
	Type            string  `json:"type"`
	RunID           string  `json:"run_id"`
	Revision        int64   `json:"revision"`
	State           string  `json:"state"`
	SourceRoot      string  `json:"source_root"`
	Destination     string  `json:"destination"`
	TotalGroups     int64   `json:"total_groups"`
	CompletedGroups int64   `json:"completed_groups"`
	AddedGroups     int64   `json:"added_groups"`
	SkippedGroups   int64   `json:"skipped_groups"`
	FailedGroups    int64   `json:"failed_groups"`
	AmbiguousGroups int64   `json:"ambiguous_groups"`
	CancelRequested bool    `json:"cancel_requested"`
	Error           *string `json:"error"`
	AmbiguityJSON   *string `json:"ambiguity_json"`
	StartedAt       string  `json:"started_at"`
	UpdatedAt       string  `json:"updated_at"`
	FinishedAt      *string `json:"finished_at"`
}

func metadataPhotoImportRunFromStore(run PhotoImportRun) (metadataPhotoImportRun, error) {
	var ambiguity *string
	if len(run.Ambiguities) > 0 {
		raw, err := json.Marshal(run.Ambiguities)
		if err != nil {
			return metadataPhotoImportRun{}, err
		}
		value := string(raw)
		ambiguity = &value
	}
	var errorText *string
	if run.Error != "" {
		errorText = &run.Error
	}
	var finished *string
	if run.FinishedAt != "" {
		finished = &run.FinishedAt
	}
	return metadataPhotoImportRun{
		Type: metadataPhotoImportRunType, RunID: run.ID, Revision: run.Revision,
		State: run.State, SourceRoot: run.SourceRoot, Destination: run.Destination,
		TotalGroups: run.TotalGroups, CompletedGroups: run.CompletedGroups,
		AddedGroups: run.AddedGroups, SkippedGroups: run.SkippedGroups,
		FailedGroups: run.FailedGroups, AmbiguousGroups: run.AmbiguousGroups,
		CancelRequested: run.CancelRequested, Error: errorText,
		AmbiguityJSON: ambiguity, StartedAt: run.StartedAt, UpdatedAt: run.UpdatedAt,
		FinishedAt: finished,
	}, nil
}

func (v metadataPhotoImportRun) toStore() (PhotoImportRun, error) {
	var ambiguities []PhotoImportAmbiguity
	if v.AmbiguityJSON != nil {
		if err := json.Unmarshal([]byte(*v.AmbiguityJSON), &ambiguities); err != nil {
			return PhotoImportRun{}, fmt.Errorf("invalid photo import ambiguity JSON: %w", err)
		}
	}
	run := PhotoImportRun{ID: v.RunID, Revision: v.Revision, State: v.State,
		SourceRoot: v.SourceRoot, Destination: v.Destination,
		TotalGroups: v.TotalGroups, CompletedGroups: v.CompletedGroups,
		AddedGroups: v.AddedGroups, SkippedGroups: v.SkippedGroups,
		FailedGroups: v.FailedGroups, AmbiguousGroups: v.AmbiguousGroups,
		CancelRequested: v.CancelRequested, Ambiguities: ambiguities,
		StartedAt: v.StartedAt, UpdatedAt: v.UpdatedAt}
	if v.Error != nil {
		run.Error = *v.Error
	}
	if v.FinishedAt != nil {
		run.FinishedAt = *v.FinishedAt
	}
	if err := validatePhotoImportRun(run); err != nil {
		return PhotoImportRun{}, err
	}
	return run, nil
}

func validatePhotoImportMetadataRecord(v metadataPhotoImportRun) error {
	if v.Type != metadataPhotoImportRunType || v.Error != nil && len(*v.Error) > maxPhotoImportTextBytes || v.AmbiguityJSON != nil && len(*v.AmbiguityJSON) > maxPhotoImportTextBytes {
		return errors.New("invalid photo import run metadata")
	}
	if v.Error != nil {
		if err := validateUTF8Field("photo import run error", *v.Error); err != nil {
			return err
		}
	}
	if v.AmbiguityJSON != nil {
		var values []PhotoImportAmbiguity
		if err := json.Unmarshal([]byte(*v.AmbiguityJSON), &values); err != nil {
			return err
		}
	}
	run, err := v.toStore()
	if err != nil {
		return err
	}
	return validatePhotoImportRun(run)
}

func exportPhotoImportMetadata(ctx context.Context, tx metadataQuerier, write metadataWrite) error {
	rows, err := tx.QueryContext(ctx, `SELECT `+photoImportRunColumns+` FROM photo_import_runs ORDER BY run_id`)
	if err != nil {
		return fmt.Errorf("exporting photo import runs: %w", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		run, err := scanPhotoImportRun(rows)
		if err != nil {
			return err
		}
		record, err := metadataPhotoImportRunFromStore(run)
		if err != nil {
			return err
		}
		if err := validatePhotoImportMetadataRecord(record); err != nil {
			return err
		}
		if err := write(record); err != nil {
			return err
		}
	}
	return rows.Err()
}

func importPhotoImportMetadataRecord(ctx context.Context, tx *sql.Tx, raw jsontext.Value) error {
	var record metadataPhotoImportRun
	if err := decodeMetadataRecord(raw, &record); err != nil {
		return err
	}
	if err := validatePhotoImportMetadataRecord(record); err != nil {
		return err
	}
	run, err := record.toStore()
	if err != nil {
		return err
	}
	if run.State == PhotoImportStateRunning || run.State == PhotoImportStateCancelRequested {
		run.State = PhotoImportStateInterrupted
		run.FinishedAt = nowRFC3339()
		run.UpdatedAt = run.FinishedAt
		run.Revision++
	}
	var ambiguity any
	if record.AmbiguityJSON != nil {
		ambiguity = *record.AmbiguityJSON
	}
	var errorText any
	if record.Error != nil {
		errorText = *record.Error
	}
	var finished any
	if run.FinishedAt != "" {
		finished = run.FinishedAt
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO photo_import_runs(
		run_id,revision,state,source_root,destination,total_groups,completed_groups,
		added_groups,skipped_groups,failed_groups,ambiguous_groups,cancel_requested,
		error,ambiguity_json,started_at,updated_at,finished_at
	) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`, run.ID, run.Revision, run.State,
		run.SourceRoot, run.Destination, run.TotalGroups, run.CompletedGroups,
		run.AddedGroups, run.SkippedGroups, run.FailedGroups, run.AmbiguousGroups,
		run.CancelRequested, errorText, ambiguity, run.StartedAt, run.UpdatedAt, finished)
	return err
}

func validatePhotoImportMetadataState(ctx context.Context, tx metadataQuerier) error {
	rows, err := tx.QueryContext(ctx, `SELECT `+photoImportRunColumns+` FROM photo_import_runs ORDER BY run_id`)
	if err != nil {
		return fmt.Errorf("validating photo import runs: %w", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		run, err := scanPhotoImportRun(rows)
		if err != nil {
			return err
		}
		if err := validatePhotoImportRun(run); err != nil {
			return err
		}
	}
	return rows.Err()
}
