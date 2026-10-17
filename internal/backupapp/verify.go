package backupapp

import (
	"context"
	"fmt"

	"go.kenn.io/kit/backup"
)

// Verify checks repository integrity and application manifest statistics in both
// quick and full modes.
func Verify(ctx context.Context, repo *backup.Repo, version string, opts backup.VerifyOptions) (*backup.VerifyResult, error) {
	app := New(version)
	result, err := backup.Verify(ctx, repo, app, opts)
	if err != nil {
		return nil, fmt.Errorf("verifying backup repository: %w", err)
	}
	if opts.Quick {
		for _, id := range result.Snapshots {
			manifest, err := repo.LoadManifest(id)
			if err != nil {
				return nil, fmt.Errorf("loading verified backup manifest %s: %w", id, err)
			}
			for _, detail := range app.CheckManifest(manifest) {
				result.Problems = append(result.Problems, backup.Problem{SnapshotID: id, Detail: detail})
			}
		}
	}
	return result, nil
}
