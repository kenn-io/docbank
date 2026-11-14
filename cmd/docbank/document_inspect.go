package main

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"uuid"

	"go.kenn.io/docbank/internal/daemonconn"
)

var inspectionProfilePattern = regexp.MustCompile(`^[a-z][a-z0-9_-]*$`)

func validateInspectionSelection(version, profile string) error {
	if err := validateInspectionVersion(version); err != nil {
		return err
	}
	return validateInspectionProfile(profile)
}

func validateInspectionVersion(version string) error {
	if !daemonconn.IsCanonicalUUIDv4(version) {
		return usageError(errors.New("version must be a canonical lowercase UUIDv4; use current_version_id from \"docbank document stat <path-or-id> --json\""))
	}
	return nil
}

func validateInspectionProfile(profile string) error {
	if len(profile) > 128 || !inspectionProfilePattern.MatchString(profile) {
		return usageError(errors.New("--profile must match [a-z][a-z0-9_-]* (1–128 characters)"))
	}
	return nil
}

func inspectionVaultID(ctx context.Context, c *daemonconn.Connection) (uuid.UUID, error) {
	identity, err := c.API().VaultIdentity(ctx)
	if err != nil {
		return uuid.Nil(), err
	}
	if identity == nil || !daemonconn.IsCanonicalUUIDv4(identity.VaultID) {
		return uuid.Nil(), errors.New("daemon returned an invalid vault identity")
	}
	return uuid.MustParse(identity.VaultID), nil
}

func inspectionProfileError(profile string, cause error) error {
	return fmt.Errorf("profile %q is not in this daemon's executable profile list; "+
		"run docbank processing profiles --json (retained evidence may still exist): %w",
		profile, cause)
}
