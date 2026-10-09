package main

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"uuid"

	"go.kenn.io/docbank/internal/apiclient"
	"go.kenn.io/docbank/internal/daemonconn"
)

var inspectionProfilePattern = regexp.MustCompile(`^[a-z][a-z0-9_-]*$`)

func validateInspectionSelection(version, profile string) error {
	if !daemonconn.IsCanonicalUUIDv4(version) {
		return usageError(errors.New("version must be a canonical lowercase UUIDv4; use current_version_id from \"docbank stat <path-or-id> --json\""))
	}
	if len(profile) > 128 || !inspectionProfilePattern.MatchString(profile) {
		return usageError(errors.New("--profile must match [a-z][a-z0-9_-]* (1–128 characters)"))
	}
	return nil
}

func inspectionVaultID(ctx context.Context, c *daemonconn.Connection) (uuid.UUID, error) {
	status, err := c.API().AuditStatus(ctx, &apiclient.AuditStatusRequestOptions{})
	if err != nil {
		return uuid.Nil(), err
	}
	if status == nil || !daemonconn.IsCanonicalUUIDv4(status.VaultID) {
		return uuid.Nil(), errors.New("audit status returned an invalid vault identity")
	}
	return uuid.MustParse(status.VaultID), nil
}

func inspectionProfileError(profile string, cause error) error {
	return fmt.Errorf("profile %q is not in this daemon's executable profile list; "+
		"run docbank processing profiles --json (retained evidence may still exist): %w",
		profile, cause)
}
