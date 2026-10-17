package store

import (
	"context"
	"database/sql"
	"encoding/json/v2"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/google/uuid"
)

// PhotoEvent tells a viewer which committed resource to refetch.
type PhotoEvent struct {
	AssetID  string `json:"asset_id,omitempty"`
	Revision int64  `json:"revision,omitzero"`
	Refetch  bool   `json:"refetch,omitzero"`
}

type photoEventFeed struct {
	sync.Mutex

	pollUntil       time.Time
	cursorID        uuid.UUID
	visibleCursor   uint64
	hiddenCursor    uint64
	captureDegraded bool
	epoch           uint64
	closed          bool
	subscribers     map[chan []PhotoEvent]struct{}
}

// SubscribePhotoEvents creates a bounded feed; overflow replaces queued IDs with a refetch.
func (s *Store) SubscribePhotoEvents() (<-chan []PhotoEvent, func()) {
	f := &s.photoEvents
	f.Lock()
	defer f.Unlock()
	ch := make(chan []PhotoEvent, 64)
	if f.closed {
		close(ch)
		return ch, func() {}
	}
	if f.subscribers == nil {
		f.subscribers = make(map[chan []PhotoEvent]struct{})
	}
	if len(f.subscribers) == 0 {
		f.captureDegraded = false
	}
	f.subscribers[ch] = struct{}{}
	f.epoch++
	var once sync.Once
	return ch, func() {
		once.Do(func() {
			f.Lock()
			if _, ok := f.subscribers[ch]; ok {
				delete(f.subscribers, ch)
				close(ch)
			}
			f.Unlock()
		})
	}
}

func (s *Store) photoCaptureState() (bool, uint64) {
	f := &s.photoEvents
	f.Lock()
	defer f.Unlock()
	return len(f.subscribers) > 0 || s.hiddenNow().Before(f.pollUntil), f.epoch
}

// PhotoChangeCursor renews capture interest for polling viewers. Resuming after
// expiry changes both cursors because uncaptured writes may have occurred.
func (s *Store) PhotoChangeCursor(ctx context.Context) (string, error) {
	_, err := s.hiddenSession(ctx, s.db)
	unlocked := err == nil
	if err != nil && !errors.Is(err, ErrHiddenLocked) && !errors.Is(err, ErrHiddenNotConfigured) {
		return "", err
	}
	return s.photoChangeCursor(unlocked), nil
}

func (s *Store) photoChangeCursor(unlocked bool) string {
	f := &s.photoEvents
	f.Lock()
	defer f.Unlock()
	if f.cursorID == uuid.Nil {
		f.cursorID = uuid.New()
	}
	now := s.hiddenNow()
	if !now.Before(f.pollUntil) {
		f.epoch++
		f.visibleCursor++
		f.hiddenCursor++
	}
	f.pollUntil = now.Add(30 * time.Second)
	if unlocked {
		return fmt.Sprintf("%s:hidden:%d", f.cursorID, f.hiddenCursor)
	}
	return fmt.Sprintf("%s:visible:%d", f.cursorID, f.visibleCursor)
}

func (s *Store) hasActivePhotoHiddenSession() bool {
	s.photoHiddenSessionsMu.Lock()
	defer s.photoHiddenSessionsMu.Unlock()
	return s.scanPhotoHiddenSessions()
}

// Caller holds photoHiddenSessionsMu. Expiry cleanup is shared with unlock.
func (s *Store) scanPhotoHiddenSessions() bool {
	now := s.hiddenNow()
	active := false
	for digest, until := range s.photoHiddenSessions {
		if until.After(now) {
			active = true
			continue
		}
		delete(s.photoHiddenSessions, digest)
	}
	return active
}

func (s *Store) closePhotoEvents() {
	f := &s.photoEvents
	f.Lock()
	defer f.Unlock()
	f.closed = true
	for ch := range f.subscribers {
		close(ch)
		delete(f.subscribers, ch)
	}
}

func (s *Store) publishPhotoChanges(events []PhotoEvent) {
	if len(events) == 0 {
		return
	}
	visibleEvents, err := s.VisiblePhotoEvents(context.Background(), events)
	visible := err != nil || len(visibleEvents) > 0
	f := &s.photoEvents
	f.Lock()
	defer f.Unlock()
	if f.closed {
		return
	}
	f.hiddenCursor++
	if visible {
		f.visibleCursor++
	}
	refetch := len(events) > 64
	for _, event := range events {
		refetch = refetch || (event.Refetch && event.AssetID == "")
	}
	for ch := range f.subscribers {
		if !refetch {
			select {
			case ch <- events:
				continue
			default:
			}
		}
	drain:
		for {
			select {
			case <-ch:
			default:
				break drain
			}
		}
		ch <- []PhotoEvent{{Refetch: true}}
	}
}

// VisiblePhotoEvents checks a delivery batch with one asset read and one Hidden session check.
func (s *Store) VisiblePhotoEvents(ctx context.Context, events []PhotoEvent) ([]PhotoEvent, error) {
	ids := make([]string, 0, len(events))
	for _, event := range events {
		if event.AssetID != "" {
			ids = append(ids, event.AssetID)
		}
	}
	if len(ids) == 0 {
		return events, nil
	}
	encoded, err := json.Marshal(ids)
	if err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx, `SELECT a.asset_id,a.revision,a.hidden_at IS NOT NULL FROM json_each(?) ids LEFT JOIN photo_assets a ON a.asset_id=ids.value`, string(encoded))
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	type assetState struct {
		revision int64
		hidden   bool
	}
	assets := make(map[string]assetState, len(ids))
	hidden := false
	for rows.Next() {
		var id sql.NullString
		var revision sql.NullInt64
		var state assetState
		if err := rows.Scan(&id, &revision, &state.hidden); err != nil {
			return nil, err
		}
		if id.Valid {
			state.revision = revision.Int64
			assets[id.String] = state
			hidden = hidden || state.hidden
		}
	}
	err = errors.Join(rows.Err(), rows.Close())
	if err != nil {
		return nil, err
	}
	unlocked, relay := false, false
	if hidden {
		_, err := s.hiddenSession(ctx, s.db)
		if err != nil && !errors.Is(err, ErrHiddenLocked) && !errors.Is(err, ErrHiddenNotConfigured) {
			return nil, err
		}
		unlocked = err == nil
		relay = !unlocked && s.hasActivePhotoHiddenSession()
	}
	visible := make([]PhotoEvent, 0, len(events))
	for _, event := range events {
		if event.AssetID != "" {
			state, exists := assets[event.AssetID]
			if !exists {
				visible = append(visible, PhotoEvent{Refetch: true})
				continue
			}
			if state.hidden && !unlocked {
				if relay {
					visible = append(visible, PhotoEvent{Refetch: true})
				}
				continue
			}
			event.Revision = state.revision
		}
		if event.Refetch {
			event = PhotoEvent{Refetch: true}
		}
		visible = append(visible, event)
	}
	return visible, nil
}
