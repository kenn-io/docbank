package api

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"
)

const (
	maxAgentSessions   = 128
	maxAgentSessionTTL = time.Hour
)

var ErrAgentSessionLimit = errors.New("agent session limit reached")

type agentSessionGrant struct {
	Operations []Operation
	SourceIDs  []string
	TTL        time.Duration
}

type issuedAgentSession struct {
	ID        string
	Token     string
	ExpiresAt time.Time
}

type agentSession struct {
	principal Principal
	tokenHash [sha256.Size]byte
	ctx       context.Context
	cancel    context.CancelFunc
	timer     *time.Timer
}

type agentSessionRegistry struct {
	mu       sync.Mutex
	vaultID  string
	now      func() time.Time
	onRevoke func(string)
	byID     map[string]*agentSession
	byToken  map[[sha256.Size]byte]*agentSession
	closed   bool
}

func newAgentSessionRegistry(vaultID string, now func() time.Time, onRevoke func(string)) *agentSessionRegistry {
	if now == nil {
		now = time.Now
	}
	return &agentSessionRegistry{
		vaultID: vaultID, now: now, onRevoke: onRevoke,
		byID:    make(map[string]*agentSession),
		byToken: make(map[[sha256.Size]byte]*agentSession),
	}
}

func (r *agentSessionRegistry) issue(grant agentSessionGrant) (issuedAgentSession, error) {
	if grant.TTL <= 0 || grant.TTL > maxAgentSessionTTL ||
		len(grant.Operations) != 1 || grant.Operations[0] != OperationRead {
		return issuedAgentSession{}, ErrOperationDenied
	}
	allowed, count, err := normalizeSourceIDs(grant.SourceIDs)
	if err != nil || count == 0 || count > MaxOperationSourceIDs {
		return issuedAgentSession{}, ErrOperationDenied
	}
	idBytes := make([]byte, 16)
	tokenBytes := make([]byte, 32)
	if _, err := rand.Read(idBytes); err != nil {
		return issuedAgentSession{}, fmt.Errorf("generating agent session ID: %w", err)
	}
	if _, err := rand.Read(tokenBytes); err != nil {
		return issuedAgentSession{}, fmt.Errorf("generating agent session token: %w", err)
	}
	id, token := hex.EncodeToString(idBytes), hex.EncodeToString(tokenBytes)
	ctx, cancel := context.WithCancel(context.Background())
	principal := Principal{
		SubjectID: "agent:" + id, CredentialKind: "agent_session",
		Audience:   "docbank:" + r.vaultID,
		Operations: append([]Operation(nil), grant.Operations...),
		SourceIDs:  allowed, GrantRevision: 1,
		ExpiresAt: r.now().UTC().Add(grant.TTL),
	}
	session := &agentSession{principal: principal, tokenHash: sha256.Sum256([]byte(token)), ctx: ctx, cancel: cancel}
	r.mu.Lock()
	expired := r.expireLocked()
	if r.closed {
		r.mu.Unlock()
		r.notifyRevoked(expired)
		cancel()
		return issuedAgentSession{}, ErrOperationDenied
	}
	if len(r.byID) >= maxAgentSessions {
		r.mu.Unlock()
		r.notifyRevoked(expired)
		cancel()
		return issuedAgentSession{}, ErrAgentSessionLimit
	}
	r.byID[id] = session
	r.byToken[session.tokenHash] = session
	session.timer = time.AfterFunc(grant.TTL, func() { r.revoke(id) })
	r.mu.Unlock()
	r.notifyRevoked(expired)
	return issuedAgentSession{ID: id, Token: token, ExpiresAt: principal.ExpiresAt}, nil
}

func (r *agentSessionRegistry) authenticate(token string) (Principal, context.Context, bool) {
	if len(token) != 64 {
		return Principal{}, nil, false
	}
	r.mu.Lock()
	expired := r.expireLocked()
	session := r.byToken[sha256.Sum256([]byte(token))]
	r.mu.Unlock()
	r.notifyRevoked(expired)
	if session == nil {
		return Principal{}, nil, false
	}
	return cloneAgentPrincipal(session.principal), session.ctx, true
}

func (r *agentSessionRegistry) CurrentGrant(_ context.Context, subjectID string) (Principal, error) {
	id, ok := strings.CutPrefix(subjectID, "agent:")
	if !ok {
		return Principal{}, ErrOperationGrantRevoked
	}
	r.mu.Lock()
	expired := r.expireLocked()
	session := r.byID[id]
	r.mu.Unlock()
	r.notifyRevoked(expired)
	if session == nil {
		return Principal{}, ErrOperationGrantRevoked
	}
	return cloneAgentPrincipal(session.principal), nil
}

func (r *agentSessionRegistry) revoke(id string) bool {
	r.mu.Lock()
	session := r.byID[id]
	if session == nil {
		r.mu.Unlock()
		return false
	}
	owner := r.removeLocked(id, session)
	r.mu.Unlock()
	r.notifyRevoked([]string{owner})
	return true
}

func (r *agentSessionRegistry) expireLocked() []string {
	now := r.now().UTC()
	var owners []string
	for id, session := range r.byID {
		if !now.Before(session.principal.ExpiresAt) {
			owners = append(owners, r.removeLocked(id, session))
		}
	}
	return owners
}

func (r *agentSessionRegistry) closeAll() {
	r.mu.Lock()
	r.closed = true
	owners := make([]string, 0, len(r.byID))
	for id, session := range r.byID {
		owners = append(owners, r.removeLocked(id, session))
	}
	r.mu.Unlock()
	r.notifyRevoked(owners)
}

func (r *agentSessionRegistry) removeLocked(id string, session *agentSession) string {
	delete(r.byID, id)
	delete(r.byToken, session.tokenHash)
	if session.timer != nil {
		session.timer.Stop()
	}
	session.cancel()
	return operationCacheKey(session.principal.SubjectID, session.principal.GrantRevision, OperationRead, nil)
}

func (r *agentSessionRegistry) notifyRevoked(owners []string) {
	if r.onRevoke != nil {
		for _, owner := range owners {
			r.onRevoke(owner)
		}
	}
}

func cloneAgentPrincipal(principal Principal) Principal {
	principal.Operations = append([]Operation(nil), principal.Operations...)
	principal.SourceIDs = append([]string(nil), principal.SourceIDs...)
	return principal
}
