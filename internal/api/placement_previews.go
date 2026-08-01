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

type placementPreviewEntry struct {
	kind      string
	plan      store.PlacementPlan
	recovery  store.StorageRecoveryPlan
	expiresAt time.Time
}

func (r *placementPreviewRegistry) issueRecovery(
	plan store.StorageRecoveryPlan,
) (string, time.Time, error) {
	var secret [32]byte
	if _, err := rand.Read(secret[:]); err != nil {
		return "", time.Time{}, fmt.Errorf("generate recovery preview secret: %w", err)
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
	r.entries[key] = placementPreviewEntry{
		kind: plan.Kind, recovery: plan, expiresAt: expiresAt,
	}
	return base64.RawURLEncoding.EncodeToString(secret[:]), expiresAt, nil
}

type placementPreviewRegistry struct {
	mu      sync.Mutex
	entries map[[sha256.Size]byte]placementPreviewEntry
}

func (r *placementPreviewRegistry) takeRecovery(
	kind, token string,
) (store.StorageRecoveryPlan, error) {
	secret, err := base64.RawURLEncoding.Strict().DecodeString(token)
	if err != nil || len(secret) != 32 {
		return store.StorageRecoveryPlan{}, errStoragePreviewUnavailable
	}
	key := sha256.Sum256(secret)
	r.mu.Lock()
	defer r.mu.Unlock()
	r.removeExpiredLocked(time.Now())
	entry, ok := r.entries[key]
	if !ok {
		return store.StorageRecoveryPlan{}, errStoragePreviewUnavailable
	}
	delete(r.entries, key)
	if entry.kind != kind {
		return store.StorageRecoveryPlan{}, errStoragePreviewUnavailable
	}
	if err := store.ValidateStorageRecoveryPlan(entry.recovery); err != nil {
		return store.StorageRecoveryPlan{}, errors.Join(errStoragePreviewUnavailable, err)
	}
	return entry.recovery, nil
}

func newPlacementPreviewRegistry() *placementPreviewRegistry {
	return &placementPreviewRegistry{
		entries: make(map[[sha256.Size]byte]placementPreviewEntry),
	}
}

func (r *placementPreviewRegistry) issue(
	kind string, plan store.PlacementPlan,
) (string, time.Time, error) {
	if kind != "place" && kind != "evacuate" {
		return "", time.Time{}, errors.New("invalid storage preview kind")
	}
	var secret [32]byte
	if _, err := rand.Read(secret[:]); err != nil {
		return "", time.Time{}, fmt.Errorf("generate placement preview secret: %w", err)
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
	r.entries[key] = placementPreviewEntry{
		kind: kind, plan: plan, expiresAt: expiresAt,
	}
	return base64.RawURLEncoding.EncodeToString(secret[:]), expiresAt, nil
}

func (r *placementPreviewRegistry) take(
	kind, token string,
) (store.PlacementPlan, error) {
	secret, err := base64.RawURLEncoding.Strict().DecodeString(token)
	if err != nil || len(secret) != 32 {
		return store.PlacementPlan{}, errStoragePreviewUnavailable
	}
	key := sha256.Sum256(secret)
	r.mu.Lock()
	defer r.mu.Unlock()
	r.removeExpiredLocked(time.Now())
	entry, ok := r.entries[key]
	if !ok {
		return store.PlacementPlan{}, errStoragePreviewUnavailable
	}
	delete(r.entries, key)
	if entry.kind != kind {
		return store.PlacementPlan{}, errStoragePreviewUnavailable
	}
	if err := store.ValidatePlacementPlan(entry.plan); err != nil {
		return store.PlacementPlan{}, errors.Join(errStoragePreviewUnavailable, err)
	}
	return entry.plan, nil
}

func (r *placementPreviewRegistry) removeExpiredLocked(now time.Time) {
	for key, entry := range r.entries {
		if !now.Before(entry.expiresAt) {
			delete(r.entries, key)
		}
	}
}
