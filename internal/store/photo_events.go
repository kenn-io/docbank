package store

import (
	"context"
	"database/sql"
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
	subscribers     map[chan PhotoEvent]struct{}
}

// SubscribePhotoEvents creates a bounded feed; overflow replaces queued IDs with a refetch.
func (s *Store) SubscribePhotoEvents() (<-chan PhotoEvent, func()) {
	f := &s.photoEvents
	f.Lock()
	defer f.Unlock()
	ch := make(chan PhotoEvent, 64)
	if f.closed {
		close(ch)
		return ch, func() {}
	}
	if f.subscribers == nil {
		f.subscribers = make(map[chan PhotoEvent]struct{})
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
		return fmt.Sprintf("%s:hidden:%d", f.cursorID, f.hiddenCursor), nil
	}
	return fmt.Sprintf("%s:visible:%d", f.cursorID, f.visibleCursor), nil
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
	visible := false
	for _, event := range events {
		delivery, err := s.VisiblePhotoEvent(context.Background(), event)
		if err != nil || delivery.Refetch || delivery.AssetID != "" {
			visible = true
			break
		}
	}
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
	for ch := range f.subscribers {
		for _, event := range events {
			if !event.Refetch || event.AssetID != "" {
				select {
				case ch <- event:
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
			ch <- PhotoEvent{Refetch: true}
			break
		}
	}
}

// VisiblePhotoEvent returns current revisions, or an empty event when Hidden access suppresses delivery.
func (s *Store) VisiblePhotoEvent(ctx context.Context, event PhotoEvent) (PhotoEvent, error) {
	gate := event.AssetID
	if gate != "" {
		var hidden bool
		var revision int64
		err := s.db.QueryRowContext(ctx, `SELECT revision,hidden_at IS NOT NULL FROM photo_assets WHERE asset_id=?`, gate).Scan(&revision, &hidden)
		if errors.Is(err, sql.ErrNoRows) {
			return PhotoEvent{Refetch: true}, nil
		}
		if err != nil {
			return PhotoEvent{}, err
		}
		if hidden {
			if _, err := s.hiddenSession(ctx, s.db); err != nil {
				if errors.Is(err, ErrHiddenLocked) || errors.Is(err, ErrHiddenNotConfigured) {
					if s.hasActivePhotoHiddenSession() {
						return PhotoEvent{Refetch: true}, nil
					}
					return PhotoEvent{}, nil
				}
				return PhotoEvent{}, err
			}
		}
		event.Revision = revision
	}
	if event.Refetch {
		return PhotoEvent{Refetch: true}, nil
	}
	return event, nil
}
