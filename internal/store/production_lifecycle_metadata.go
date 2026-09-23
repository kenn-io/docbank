package store

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"hash"
	"slices"
	"strconv"
	"strings"

	documentproduction "go.kenn.io/docbank/document/production"
	"go.kenn.io/docbank/internal/canonical"
)

const (
	metadataProductionLifecycleType         = "production_lifecycle"
	metadataProductionLifecycleManifestType = "production_lifecycle_manifest"
	maxProductionLifecycleRowBytes          = 256 << 20
	productionLifecycleStorageSchemaVersion = 30
)

// The order is also the foreign-key import order. Keep all current lifecycle
// tables here, including tables that are empty in a particular vault.
var productionLifecycleTables = [...]string{
	"production_text_maps", "production_text_map_frames", "production_catalog_entries",
	"production_sets", "production_revisions", "production_members",
	"production_member_policy_facts", "production_revision_email_publications",
	"production_revision_gate_authority", "production_decisions", "production_operations",
	"production_audit_evidence", "production_finalized_revisions", "production_jobs",
	"production_job_artifacts", "production_job_render_plans", "production_job_page_stages",
}

type metadataProductionLifecycle struct {
	Type     string   `json:"type"`
	Kind     string   `json:"kind"`
	Values   []string `json:"values"`
	Checksum string   `json:"checksum"`
}

type metadataProductionLifecycleManifest struct {
	Type     string  `json:"type"`
	Counts   []int64 `json:"counts"`
	Checksum string  `json:"checksum"`
}

type productionLifecycleColumn struct {
	name, kind string
	pk         int
}

func productionLifecycleColumns(ctx context.Context, q metadataQuerier, table string) ([]productionLifecycleColumn, error) {
	rows, err := q.QueryContext(ctx, `PRAGMA table_info(`+table+`)`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var columns []productionLifecycleColumn
	for rows.Next() {
		var ordinal, notNull int
		var name, kind string
		var defaultValue any
		var pk int
		if err := rows.Scan(&ordinal, &name, &kind, &notNull, &defaultValue, &pk); err != nil {
			return nil, err
		}
		if name == "" || (kind != "TEXT" && kind != "INTEGER" && kind != "BLOB") {
			return nil, fmt.Errorf("unsupported production lifecycle column %s.%s (%s)", table, name, kind)
		}
		columns = append(columns, productionLifecycleColumn{name: name, kind: kind, pk: pk})
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(columns) == 0 || !slices.ContainsFunc(columns, func(c productionLifecycleColumn) bool { return c.pk > 0 }) {
		return nil, fmt.Errorf("production lifecycle table %s has no primary key", table)
	}
	return columns, nil
}

func productionLifecycleOrder(columns []productionLifecycleColumn) string {
	keys := slices.Clone(columns)
	slices.SortFunc(keys, func(a, b productionLifecycleColumn) int { return a.pk - b.pk })
	var names []string
	for _, column := range keys {
		if column.pk > 0 {
			names = append(names, column.name)
		}
	}
	return strings.Join(names, ",")
}

func productionLifecycleValue(kind string, value any) (string, error) {
	if value == nil {
		return "n:", nil
	}
	switch kind {
	case "TEXT":
		if text, ok := value.(string); ok {
			return "t:" + text, nil
		}
	case "INTEGER":
		if number, ok := value.(int64); ok {
			return "i:" + strconv.FormatInt(number, 10), nil
		}
	case "BLOB":
		if data, ok := value.([]byte); ok {
			return "b:" + base64.StdEncoding.EncodeToString(data), nil
		}
	}
	return "", fmt.Errorf("invalid production lifecycle %s value %T", kind, value)
}

func productionLifecycleSQLValue(kind, value string) (any, error) {
	if value == "n:" {
		return nil, nil //nolint:nilnil // nil is the SQL NULL value for an optional column.
	}
	if len(value) < 2 || value[1] != ':' {
		return nil, errors.New("invalid production lifecycle value prefix")
	}
	switch kind {
	case "TEXT":
		if value[0] == 't' {
			return value[2:], nil
		}
	case "INTEGER":
		if value[0] == 'i' {
			number, err := strconv.ParseInt(value[2:], 10, 64)
			if err == nil && strconv.FormatInt(number, 10) == value[2:] {
				return number, nil
			}
		}
	case "BLOB":
		if value[0] == 'b' && len(value) <= maxProductionLifecycleRowBytes {
			data, err := base64.StdEncoding.Strict().DecodeString(value[2:])
			if err == nil {
				return data, nil
			}
		}
	}
	return nil, fmt.Errorf("invalid production lifecycle %s value", kind)
}

func writeProductionLifecycleDigest(digest hash.Hash, table, checksum string) {
	_, _ = digest.Write([]byte(table))
	_, _ = digest.Write([]byte{'\n'})
	_, _ = digest.Write([]byte(checksum))
	_, _ = digest.Write([]byte{'\n'})
}

func exportProductionLifecycleMetadata(ctx context.Context, q metadataQuerier, write metadataWrite) (metadataProductionLifecycleManifest, error) {
	manifest := metadataProductionLifecycleManifest{Type: metadataProductionLifecycleManifestType,
		Counts: make([]int64, len(productionLifecycleTables))}
	digest := sha256.New()
	for index, table := range productionLifecycleTables {
		columns, err := productionLifecycleColumns(ctx, q, table)
		if err != nil {
			return manifest, err
		}
		writeProductionLifecycleDigest(digest, table, "")
		rows, err := q.QueryContext(ctx, `SELECT * FROM `+table+` ORDER BY `+productionLifecycleOrder(columns))
		if err != nil {
			return manifest, err
		}
		defer func() { _ = rows.Close() }()
		for rows.Next() {
			values := make([]any, len(columns))
			addresses := make([]any, len(columns))
			for i := range values {
				addresses[i] = &values[i]
			}
			if err := rows.Scan(addresses...); err != nil {
				_ = rows.Close()
				return manifest, err
			}
			record := metadataProductionLifecycle{Type: metadataProductionLifecycleType, Kind: table,
				Values: make([]string, len(columns))}
			for i, value := range values {
				record.Values[i], err = productionLifecycleValue(columns[i].kind, value)
				if err != nil {
					_ = rows.Close()
					return manifest, err
				}
			}
			encoded, err := canonical.Marshal(record.Values)
			if err != nil || len(encoded) > maxProductionLifecycleRowBytes {
				_ = rows.Close()
				return manifest, errors.New("production lifecycle row exceeds metadata limit")
			}
			record.Checksum = digestProductionBytes(encoded)
			writeProductionLifecycleDigest(digest, table, record.Checksum)
			if err := write(record); err != nil {
				_ = rows.Close()
				return manifest, err
			}
			manifest.Counts[index]++
		}
		if err := errors.Join(rows.Err(), rows.Close()); err != nil {
			return manifest, err
		}
	}
	manifest.Checksum = hex.EncodeToString(digest.Sum(nil))
	return manifest, nil
}

func importProductionLifecycleMetadata(ctx context.Context, tx *sql.Tx, raw jsontext.Value) error {
	if len(raw) > 2*maxProductionLifecycleRowBytes {
		return errors.New("production lifecycle metadata row exceeds input limit")
	}
	var record metadataProductionLifecycle
	if err := decodeMetadataRecord(raw, &record); err != nil {
		return err
	}
	if record.Type != metadataProductionLifecycleType || !slices.Contains(productionLifecycleTables[:], record.Kind) {
		return errors.New("unknown production lifecycle table")
	}
	encoded, err := canonical.Marshal(record.Values)
	if err != nil || len(encoded) > maxProductionLifecycleRowBytes || digestProductionBytes(encoded) != record.Checksum {
		return errors.New("invalid production lifecycle checksum")
	}
	columns, err := productionLifecycleColumns(ctx, tx, record.Kind)
	if err != nil {
		return err
	}
	if record.Kind == "production_job_artifacts" && len(record.Values) == 4 {
		// Early v1 lifecycle snapshots predate the typed root. Derive it only
		// from a bounded, canonical, validated artifact; keep the input row's
		// checksum for its original manifest before normalizing the columns.
		if len(record.Values[2]) > 2*maxProductionArtifactBytes+2 {
			return errors.New("production artifact exceeds input limit")
		}
		value, err := productionLifecycleSQLValue("BLOB", record.Values[2])
		if err != nil {
			return err
		}
		raw, ok := value.([]byte)
		if !ok || len(raw) == 0 || len(raw) > maxProductionArtifactBytes {
			return errors.New("invalid legacy production artifact")
		}
		artifact, err := canonical.Decode[documentproduction.Artifact](raw)
		if err != nil || len(record.Values[1]) < 2 || record.Values[1] != "t:"+artifact.ID {
			return errors.New("invalid legacy production artifact identity")
		}
		if _, _, err := documentproduction.CanonicalArtifactManifest(documentproduction.ArtifactManifest{
			Contract: documentproduction.ArtifactManifestContractV1, Artifacts: []documentproduction.Artifact{artifact},
		}); err != nil {
			return fmt.Errorf("invalid legacy production artifact: %w", err)
		}
		if encoded, err := canonical.Marshal(artifact); err != nil || !bytes.Equal(encoded, raw) {
			return errors.New("legacy production artifact is not canonical")
		}
		record.Values = []string{record.Values[0], record.Values[1], "t:" + artifact.SHA256,
			"i:" + strconv.FormatInt(artifact.Size, 10), record.Values[2], record.Values[3]}
	}
	if len(record.Values) != len(columns) {
		return errors.New("production lifecycle column count mismatch")
	}
	values := make([]any, len(columns))
	marks := make([]string, len(columns))
	names := make([]string, len(columns))
	for index, column := range columns {
		values[index], err = productionLifecycleSQLValue(column.kind, record.Values[index])
		if err != nil {
			return err
		}
		names[index], marks[index] = column.name, "?"
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO `+record.Kind+`(`+strings.Join(names, ",")+
		`) VALUES(`+strings.Join(marks, ",")+`)`, values...)
	return err
}

func decodeProductionLifecycleManifest(raw jsontext.Value) (metadataProductionLifecycleManifest, error) {
	var record metadataProductionLifecycleManifest
	if len(raw) > 4096 {
		return record, errors.New("production lifecycle manifest exceeds input limit")
	}
	if err := json.Unmarshal(raw, &record, json.RejectUnknownMembers(true)); err != nil {
		return record, err
	}
	if record.Type != metadataProductionLifecycleManifestType || len(record.Counts) != len(productionLifecycleTables) {
		return record, errors.New("invalid production lifecycle manifest")
	}
	for _, count := range record.Counts {
		if count < 0 {
			return record, errors.New("negative production lifecycle count")
		}
	}
	return record, nil
}
