package store

import (
	"context"
	"database/sql"
	"errors"
	"sync"
)

// PhotoEvent tells a viewer which committed resource to refetch.
type PhotoEvent struct {
	AssetID  string `json:"asset_id,omitempty"`
	Revision int64  `json:"revision,omitzero"`
	Refetch  bool   `json:"refetch,omitzero"`
}

type photoEventFeed struct {
	sync.Mutex

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
	return len(f.subscribers) > 0, f.epoch
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
	f := &s.photoEvents
	f.Lock()
	defer f.Unlock()
	if f.closed {
		return
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
		if event.AssetID != "" {
			event.Revision = revision
		}
	}
	if event.Refetch {
		return PhotoEvent{Refetch: true}, nil
	}
	return event, nil
}
