package api

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"sync"
	"time"

	"go.kenn.io/kit/packstore"

	"go.kenn.io/docbank/internal/config"
	"go.kenn.io/docbank/internal/store"
)

const storagePreviewTTL = 10 * time.Minute

var errStoragePreviewUnavailable = errors.New(
	"storage preview is expired, used, or from another daemon",
)

type storageRegistrationPlan struct {
	store        store.BlobStore
	binding      config.StoreBindingConfig
	expected     *packstore.Ownership
	markerAction string
	takeover     bool
}

type storagePreviewEntry struct {
	plan      storageRegistrationPlan
	expiresAt time.Time
}

type storagePreviewRegistry struct {
	mu      sync.Mutex
	entries map[[sha256.Size]byte]storagePreviewEntry
}

func newStoragePreviewRegistry() *storagePreviewRegistry {
	return &storagePreviewRegistry{
		entries: make(map[[sha256.Size]byte]storagePreviewEntry),
	}
}

func (r *storagePreviewRegistry) issue(
	plan storageRegistrationPlan,
) (string, time.Time, error) {
	var secret [32]byte
	if _, err := rand.Read(secret[:]); err != nil {
		return "", time.Time{}, fmt.Errorf("generate storage preview secret: %w", err)
	}
	key := sha256.Sum256(secret[:])
	expiresAt := time.Now().Add(storagePreviewTTL)
	r.mu.Lock()
	defer r.mu.Unlock()
	r.removeExpiredLocked(time.Now())
	if len(r.entries) >= 64 {
		var oldest [sha256.Size]byte
		var oldestTime time.Time
		for candidate, entry := range r.entries {
			if oldestTime.IsZero() || entry.expiresAt.Before(oldestTime) {
				oldest, oldestTime = candidate, entry.expiresAt
			}
		}
		delete(r.entries, oldest)
	}
	r.entries[key] = storagePreviewEntry{plan: plan, expiresAt: expiresAt}
	return base64.RawURLEncoding.EncodeToString(secret[:]), expiresAt, nil
}

func (r *storagePreviewRegistry) take(token string) (storageRegistrationPlan, error) {
	secret, err := base64.RawURLEncoding.Strict().DecodeString(token)
	if err != nil || len(secret) != 32 {
		return storageRegistrationPlan{}, errStoragePreviewUnavailable
	}
	key := sha256.Sum256(secret)
	r.mu.Lock()
	defer r.mu.Unlock()
	r.removeExpiredLocked(time.Now())
	entry, ok := r.entries[key]
	if !ok {
		return storageRegistrationPlan{}, errStoragePreviewUnavailable
	}
	delete(r.entries, key)
	return entry.plan, nil
}

func (r *storagePreviewRegistry) removeExpiredLocked(now time.Time) {
	for key, entry := range r.entries {
		if !entry.expiresAt.After(now) {
			delete(r.entries, key)
		}
	}
}
