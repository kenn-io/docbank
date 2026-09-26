package daemonconn

import (
	"context"
	"encoding/hex"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"strings"
	"time"

	"go.kenn.io/docbank/internal/apiclient"
)

const AgentSessionFileEnv = "DOCBANK_AGENT_SESSION_FILE"

const maxAgentSessionFileBytes = 4096

var ErrAgentSessionFile = errors.New("agent session file is unavailable or invalid")

// AgentSessionFile binds a short-lived token to one already-running daemon.
// It is private caller state; it is never written to the vault or a PR.
type AgentSessionFile struct {
	Version   int       `json:"version"`
	Origin    string    `json:"origin"`
	Token     string    `json:"token"`
	ExpiresAt time.Time `json:"expires_at"`
}

// ConnectAgentSessionFile reads one private, regular file and attaches without
// discovery, daemon launch, or a master-key fallback.
func ConnectAgentSessionFile(ctx context.Context, path string) (*Connection, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if !filepath.IsAbs(path) {
		return nil, ErrAgentSessionFile
	}
	file, err := openPrivateAgentSessionFile(path)
	if err != nil {
		return nil, ErrAgentSessionFile
	}
	defer func() { _ = file.Close() }()
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() ||
		info.Size() < 1 || info.Size() > maxAgentSessionFileBytes {
		return nil, ErrAgentSessionFile
	}
	raw, err := io.ReadAll(io.LimitReader(file, maxAgentSessionFileBytes+1))
	if err != nil || len(raw) == 0 || len(raw) > maxAgentSessionFileBytes {
		return nil, ErrAgentSessionFile
	}
	var binding AgentSessionFile
	if err := json.Unmarshal(raw, &binding, json.RejectUnknownMembers(true)); err != nil ||
		binding.Version != 1 || !binding.ExpiresAt.After(time.Now()) ||
		!validAgentSessionToken(binding.Token) {
		return nil, ErrAgentSessionFile
	}
	connection, err := AttachAgentSession(binding.Origin, binding.Token)
	if err != nil {
		return nil, fmt.Errorf("%w: invalid daemon origin", ErrAgentSessionFile)
	}
	return connection, nil
}

func validAgentSessionToken(token string) bool {
	if len(token) != 64 || strings.ToLower(token) != token {
		return false
	}
	_, err := hex.DecodeString(token)
	return err == nil
}

// IssueAgentSession asks the master-authenticated daemon for one read-only
// grant. The returned token must be placed in a private file by the caller.
func (c *Connection) IssueAgentSession(
	ctx context.Context, sourceIDs []string, ttl time.Duration,
) (AgentSessionFile, string, error) {
	if c == nil || c.key == "" || ttl <= 0 || ttl > time.Hour || ttl%time.Second != 0 || len(sourceIDs) == 0 {
		return AgentSessionFile{}, "", ErrAgentSessionFile
	}
	issued, err := c.API().IssueAgentSession(ctx, &apiclient.IssueAgentSessionRequestOptions{
		Body: &apiclient.IssueAgentSessionBody{
			Operations: []string{"read"}, SourceIds: sourceIDs, TTLSeconds: int64(ttl / time.Second),
		},
	})
	if err != nil {
		return AgentSessionFile{}, "", err
	}
	binding := AgentSessionFile{Version: 1, Origin: c.base, Token: issued.Token, ExpiresAt: issued.ExpiresAt}
	if len(issued.ID) != 32 || !issued.ExpiresAt.After(time.Now()) ||
		!validAgentSessionToken(issued.Token) {
		return AgentSessionFile{}, "", ErrAgentSessionFile
	}
	if _, err := AttachAgentSession(binding.Origin, binding.Token); err != nil {
		return AgentSessionFile{}, "", ErrAgentSessionFile
	}
	return binding, issued.ID, nil
}

// RevokeAgentSession requires a master-authenticated connection.
func (c *Connection) RevokeAgentSession(ctx context.Context, id string) error {
	if c == nil || c.key == "" || len(id) != 32 || strings.ToLower(id) != id {
		return ErrAgentSessionFile
	}
	if _, err := hex.DecodeString(id); err != nil {
		return ErrAgentSessionFile
	}
	_, err := c.API().RevokeAgentSession(ctx, &apiclient.RevokeAgentSessionRequestOptions{
		PathParams: &apiclient.RevokeAgentSessionPath{ID: id},
	})
	return err
}
