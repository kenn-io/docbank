package api

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"sync"
	"time"

	"go.kenn.io/docbank/internal/store"
)

const (
	auditPreviewTTL      = 10 * time.Minute
	maxAuditPreviewPlans = 64
)

var errAuditPreviewUnavailable = errors.New("audit enrollment preview is expired, used, or from another daemon")

type auditPreviewEntry struct {
	plan      *store.AuditEnrollmentPlan
	expiresAt time.Time
}

// auditPreviewRegistry keeps irreversible enrollment plans daemon-local,
// bounded, short-lived, and one-use. Only a SHA-256 key for the random wire
// secret is retained; restart intentionally invalidates every preview.
type auditPreviewRegistry struct {
	mu      sync.Mutex
	entries map[[sha256.Size]byte]auditPreviewEntry
	now     func() time.Time
	ttl     time.Duration
}

func newAuditPreviewRegistry() *auditPreviewRegistry {
	return &auditPreviewRegistry{
		entries: make(map[[sha256.Size]byte]auditPreviewEntry),
		now:     time.Now,
		ttl:     auditPreviewTTL,
	}
}

func (registry *auditPreviewRegistry) issue(
	plan *store.AuditEnrollmentPlan,
) (string, time.Time, error) {
	secret := make([]byte, 32)
	if _, err := rand.Read(secret); err != nil {
		return "", time.Time{}, fmt.Errorf("generating audit preview secret: %w", err)
	}
	key := sha256.Sum256(secret)
	now := registry.now()
	expiresAt := now.Add(registry.ttl)
	registry.mu.Lock()
	defer registry.mu.Unlock()
	registry.removeExpiredLocked(now)
	if len(registry.entries) >= maxAuditPreviewPlans {
		var oldestKey [sha256.Size]byte
		var oldestTime time.Time
		for candidate, entry := range registry.entries {
			if oldestTime.IsZero() || entry.expiresAt.Before(oldestTime) {
				oldestKey, oldestTime = candidate, entry.expiresAt
			}
		}
		delete(registry.entries, oldestKey)
	}
	registry.entries[key] = auditPreviewEntry{plan: plan, expiresAt: expiresAt}
	return base64.RawURLEncoding.EncodeToString(secret), expiresAt, nil
}

func (registry *auditPreviewRegistry) take(token string) (*store.AuditEnrollmentPlan, error) {
	secret, err := base64.RawURLEncoding.Strict().DecodeString(token)
	if err != nil || len(secret) != 32 {
		return nil, errAuditPreviewUnavailable
	}
	key := sha256.Sum256(secret)
	now := registry.now()
	registry.mu.Lock()
	defer registry.mu.Unlock()
	registry.removeExpiredLocked(now)
	entry, ok := registry.entries[key]
	if !ok {
		return nil, errAuditPreviewUnavailable
	}
	delete(registry.entries, key)
	return entry.plan, nil
}

func (registry *auditPreviewRegistry) removeExpiredLocked(now time.Time) {
	for key, entry := range registry.entries {
		if !entry.expiresAt.After(now) {
			delete(registry.entries, key)
		}
	}
}
