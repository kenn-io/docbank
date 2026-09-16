package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

const packagePreflightRetention = 24 * time.Hour

type PackagePreflightRecord struct {
	PreflightID           string
	Owner                 string
	SourceKind            string
	SourceRef             string
	ProfileSHA256         string
	MappingSHA256         string
	ManifestSHA256        string
	ManifestBlobSHA256    string
	DiagnosticsBlobSHA256 string
	CanonicalJSON         []byte
	DiagnosticsJSON       []byte
	Blocking              bool
	CreatedAt             string
	ExpiresAt             string
}

func (s *Store) PutPackagePreflight(ctx context.Context, record PackagePreflightRecord) (PackagePreflightRecord, error) {
	if err := validatePackagePreflight(&record); err != nil {
		return PackagePreflightRecord{}, err
	}
	err := s.withStorageTx(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `INSERT INTO package_preflights(
			preflight_id,owner,source_kind,source_ref,profile_sha256,mapping_sha256,
			manifest_sha256,manifest_blob_sha256,diagnostics_blob_sha256,canonical_json,
			diagnostics_json,blocking,created_at,expires_at)
			VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
			record.PreflightID, record.Owner, record.SourceKind, record.SourceRef,
			record.ProfileSHA256, record.MappingSHA256, record.ManifestSHA256,
			record.ManifestBlobSHA256, sql.NullString{String: record.DiagnosticsBlobSHA256, Valid: record.DiagnosticsBlobSHA256 != ""},
			record.CanonicalJSON, record.DiagnosticsJSON, record.Blocking,
			record.CreatedAt, record.ExpiresAt)
		if err != nil {
			return fmt.Errorf("storing package preflight: %w", err)
		}
		for _, hash := range []string{record.ManifestBlobSHA256, record.DiagnosticsBlobSHA256} {
			if hash == "" {
				continue
			}
			if _, err := tx.ExecContext(ctx, `DELETE FROM rendition_blob_staging WHERE blob_hash=?`, hash); err != nil {
				return fmt.Errorf("transferring package preflight blob hold: %w", err)
			}
		}
		return nil
	})
	if err != nil {
		return PackagePreflightRecord{}, err
	}
	return record, nil
}

func validatePackagePreflight(record *PackagePreflightRecord) error {
	if err := validateUUIDv4(record.PreflightID); err != nil {
		return fmt.Errorf("invalid package preflight id: %w", err)
	}
	if record.Owner == "" || record.SourceKind != "root" || record.SourceRef == "" {
		return errors.New("invalid package preflight source")
	}
	for label, value := range map[string]string{
		"profile": record.ProfileSHA256, "mapping": record.MappingSHA256,
		"manifest": record.ManifestSHA256, "manifest blob": record.ManifestBlobSHA256,
	} {
		if err := validateCatalogSHA256(value, label+" SHA-256"); err != nil {
			return err
		}
	}
	if record.ManifestBlobSHA256 != record.ManifestSHA256 {
		return errors.New("package preflight manifest blob must match manifest SHA-256")
	}
	if record.DiagnosticsBlobSHA256 != "" {
		if err := validateCatalogSHA256(record.DiagnosticsBlobSHA256, "diagnostics blob SHA-256"); err != nil {
			return err
		}
	}
	if record.CanonicalJSON == nil || record.DiagnosticsJSON == nil {
		return errors.New("package preflight summaries are required")
	}
	if record.CreatedAt == "" {
		record.CreatedAt = nowRFC3339()
	}
	created, err := time.Parse(timestampLayout, record.CreatedAt)
	if err != nil {
		return fmt.Errorf("invalid package preflight created_at: %w", err)
	}
	if record.ExpiresAt == "" {
		record.ExpiresAt = created.Add(packagePreflightRetention).Format(timestampLayout)
	}
	if err := validateMetadataTime("package preflight expires_at", record.ExpiresAt); err != nil {
		return err
	}
	expires, _ := time.Parse(timestampLayout, record.ExpiresAt)
	if !expires.After(created) {
		return errors.New("package preflight expiry must follow creation")
	}
	return nil
}

func (s *Store) PackagePreflight(ctx context.Context, owner, preflightID string) (PackagePreflightRecord, error) {
	var record PackagePreflightRecord
	var diagnostics sql.NullString
	err := s.db.QueryRowContext(ctx, `SELECT preflight_id,owner,source_kind,source_ref,
		profile_sha256,mapping_sha256,manifest_sha256,manifest_blob_sha256,
		diagnostics_blob_sha256,canonical_json,diagnostics_json,blocking,created_at,expires_at
		FROM package_preflights WHERE owner=? AND preflight_id=? AND expires_at>?`, owner, preflightID, nowRFC3339()).Scan(
		&record.PreflightID, &record.Owner, &record.SourceKind, &record.SourceRef,
		&record.ProfileSHA256, &record.MappingSHA256, &record.ManifestSHA256,
		&record.ManifestBlobSHA256, &diagnostics, &record.CanonicalJSON,
		&record.DiagnosticsJSON, &record.Blocking, &record.CreatedAt, &record.ExpiresAt)
	if errors.Is(err, sql.ErrNoRows) {
		return PackagePreflightRecord{}, ErrNotFound
	}
	if err != nil {
		return PackagePreflightRecord{}, fmt.Errorf("reading package preflight: %w", err)
	}
	record.DiagnosticsBlobSHA256 = diagnostics.String
	return record, nil
}

func (s *Store) ExpirePackagePreflights(ctx context.Context, now string) (int64, error) {
	if now == "" {
		now = nowRFC3339()
	}
	if err := validateMetadataTime("preflight expiry", now); err != nil {
		return 0, err
	}
	var count int64
	err := s.withStorageTx(ctx, func(tx *sql.Tx) error {
		result, err := tx.ExecContext(ctx, `DELETE FROM package_preflights WHERE expires_at <= ?`, now)
		if err != nil {
			return fmt.Errorf("expiring package preflights: %w", err)
		}
		count, err = result.RowsAffected()
		return err
	})
	return count, err
}
