package store

import (
	"context"
	"database/sql"
	"errors"
	"sync"
)

type photoAudience uint8

const (
	photoPublic photoAudience = 1 << iota
	photoHidden
	photoControl
	photoDegraded
)

func (e PhotoEvent) audiences() photoAudience {
	if e.audience != 0 {
		return e.audience
	}
	if e.Refetch {
		return photoControl
	}
	return photoPublic
}

// PhotoEvent tells a viewer which committed resource to refetch.
type PhotoEvent struct {
	AssetID  string `json:"asset_id,omitempty"`
	AlbumID  string `json:"album_id,omitempty"`
	Revision int64  `json:"revision,omitzero"`
	Refetch  bool   `json:"refetch,omitzero"`
	audience photoAudience
}

type photoEventFeed struct {
	sync.Mutex

	captureDegraded bool
	epoch           uint64
	closed          bool
	cleanupPending  bool
	subscribers     map[chan PhotoEvent]string
}

// SubscribePhotoEvents creates a bounded feed; overflow replaces queued IDs with a refetch.
func (s *Store) SubscribePhotoEvents(ctx context.Context) (<-chan PhotoEvent, func()) {
	token, _ := ctx.Value(hiddenTokenKey{}).(string)
	f := &s.photoEvents
	f.Lock()
	defer f.Unlock()
	ch := make(chan PhotoEvent, 64)
	if f.closed {
		close(ch)
		return ch, func() {}
	}
	if f.subscribers == nil {
		f.subscribers = make(map[chan PhotoEvent]string)
	}
	if len(f.subscribers) == 0 {
		f.captureDegraded = false
	}
	f.subscribers[ch] = hiddenTokenDigest(token)
	f.epoch++
	var once sync.Once
	return ch, func() {
		once.Do(func() {
			f.Lock()
			if _, ok := f.subscribers[ch]; ok {
				delete(f.subscribers, ch)
				close(ch)
			}
			idle := len(f.subscribers) == 0 && !f.closed
			if idle {
				f.cleanupPending = true
			}
			f.Unlock()
			if idle {
				s.clearIdlePhotoCapture()
			}
		})
	}
}

// Session digests let capture stop at expiry or revocation without database reads.
func (s *Store) photoCaptureState() (bool, bool, uint64) {
	s.photoHiddenSessionsMu.Lock()
	defer s.photoHiddenSessionsMu.Unlock()
	f := &s.photoEvents
	f.Lock()
	defer f.Unlock()
	hidden := false
	now := s.hiddenNow()
	for _, digest := range f.subscribers {
		hidden = hidden || s.photoHiddenSessions[digest].After(now)
	}
	return len(f.subscribers) > 0, hidden, f.epoch
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
		for i, event := range events {
			if !event.Refetch {
				select {
				case ch <- event:
					continue
				default:
				}
			}
			audience := event.audiences()
		drain:
			for {
				select {
				case queued := <-ch:
					audience |= queued.audiences()
				default:
					break drain
				}
			}
			for _, remaining := range events[i+1:] {
				audience |= remaining.audiences()
			}
			ch <- PhotoEvent{Refetch: true, audience: audience}
			break
		}
	}
}

// VisiblePhotoEvent returns current revisions, or an empty event when Hidden access suppresses delivery.
func (s *Store) VisiblePhotoEvent(ctx context.Context, event PhotoEvent) (PhotoEvent, error) {
	audience := event.audiences()
	if audience == photoHidden {
		if _, err := s.hiddenSession(ctx, s.db); err != nil {
			if errors.Is(err, ErrHiddenLocked) || errors.Is(err, ErrHiddenNotConfigured) {
				return PhotoEvent{}, nil
			}
			return PhotoEvent{}, err
		}
	}
	if event.Refetch {
		return PhotoEvent{Refetch: true}, nil
	}
	event.audience = 0
	if event.AssetID != "" {
		var hidden bool
		err := s.db.QueryRowContext(ctx, `SELECT revision,hidden_at IS NOT NULL FROM photo_assets WHERE asset_id=?`, event.AssetID).Scan(&event.Revision, &hidden)
		if errors.Is(err, sql.ErrNoRows) {
			return PhotoEvent{Refetch: true}, nil
		}
		if err != nil {
			return PhotoEvent{}, err
		}
		if hidden && audience != photoHidden {
			if _, err := s.hiddenSession(ctx, s.db); errors.Is(err, ErrHiddenLocked) || errors.Is(err, ErrHiddenNotConfigured) {
				return PhotoEvent{Refetch: true}, nil
			} else if err != nil {
				return PhotoEvent{}, err
			}
		}
	}
	if event.AlbumID != "" {
		set, err := photoSetByID(ctx, s.db, event.AlbumID)
		if errors.Is(err, ErrNotFound) {
			return PhotoEvent{Refetch: true}, nil
		}
		if err != nil {
			return PhotoEvent{}, err
		}
		event.Revision = set.Revision
	}
	return event, nil
}
