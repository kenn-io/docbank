package daemonconn

import (
	"context"
	"encoding/hex"
	"errors"
	"strings"

	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/apiclient"
)

// WebSignIns lists process-local browser key logins through the ownership-proven daemon.
func (c *Connection) WebSignIns(ctx context.Context) ([]api.WebSignInRecord, error) {
	page, err := c.API().ListWebSignIns(ctx)
	if err != nil {
		return nil, err
	}
	records := make([]api.WebSignInRecord, 0, len(page.Items))
	for _, record := range page.Items {
		if !validWebSignInID(record.ID) || record.CreatedAt.IsZero() || !record.ExpiresAt.After(record.CreatedAt) {
			return nil, errors.New("daemon returned an invalid browser sign-in record")
		}
		records = append(records, record)
	}
	return records, nil
}

// RevokeWebSignIn revokes one browser key login.
func (c *Connection) RevokeWebSignIn(ctx context.Context, id string) error {
	if !validWebSignInID(id) {
		return errors.New("browser session ID must be a lowercase SHA-256 digest")
	}
	_, err := c.API().RevokeWebSignIn(ctx, &apiclient.RevokeWebSignInRequestOptions{PathParams: &apiclient.RevokeWebSignInPath{ID: id}})
	return err
}

func validWebSignInID(id string) bool {
	raw, err := hex.DecodeString(id)
	return err == nil && len(raw) == 32 && id == strings.ToLower(id)
}
