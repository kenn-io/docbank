package store

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	"golang.org/x/crypto/argon2"
	"golang.org/x/text/unicode/norm"
)

const hiddenArgonMemory, hiddenArgonTime, hiddenArgonThreads = 19456, 2, 1

var (
	hiddenArgonParameters = fmt.Sprintf("m=%d,t=%d,p=%d", hiddenArgonMemory, hiddenArgonTime, hiddenArgonThreads)

	ErrHiddenLocked          = errors.New("hidden photos are locked")
	ErrHiddenNotConfigured   = errors.New("configure a hidden photos passcode first")
	ErrHiddenConfigured      = errors.New("hidden photos already have a passcode")
	ErrHiddenPasscode        = errors.New("incorrect hidden photos passcode")
	ErrInvalidHiddenPasscode = errors.New("passcode must be 1 to 1024 bytes without line breaks")
)

type HiddenLockoutError struct{ Until time.Time }

func (e *HiddenLockoutError) Error() string {
	if e.Until.IsZero() {
		return "too many attempts; hidden photos are temporarily locked"
	}
	return "too many attempts; hidden photos are locked until " + e.Until.Format(time.RFC3339)
}

type hiddenTokenKey struct{}

func WithPhotoHiddenToken(ctx context.Context, token string) context.Context {
	return context.WithValue(ctx, hiddenTokenKey{}, token)
}

// PhotoHiddenState reports deadlines both as server timestamps and as whole
// seconds remaining, so clients can count down without trusting their clock.
type PhotoHiddenState struct {
	ChangeCursor string  `json:"change_cursor,omitzero"`
	ChangeID     string  `json:"change_id,omitzero"`
	Configured   bool    `json:"configured"`
	ExpiresAt    *string `json:"expires_at,omitzero"`
	ExpiresIn    *int64  `json:"expires_in,omitzero"`
	LockedUntil  *string `json:"locked_until,omitzero"`
	LockedFor    *int64  `json:"locked_for,omitzero"`
}

func hiddenSecondsUntil(now, deadline time.Time) *int64 {
	return new(int64((deadline.Sub(now) + time.Second - 1) / time.Second))
}

func validHiddenPasscode(passcode string) error {
	if len(passcode) < 1 || len(passcode) > 1024 || strings.ContainsAny(passcode, "\r\n") {
		return ErrInvalidHiddenPasscode
	}
	return nil
}

func hiddenHashParts(encoded string) ([]byte, []byte, error) {
	parts := strings.Split(encoded, "$")
	if len(parts) != 4 || parts[0] != "argon2id" || parts[1] != hiddenArgonParameters {
		return nil, nil, errors.New("invalid hidden credential hash")
	}
	salt, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil || len(salt) != 16 {
		return nil, nil, errors.New("invalid hidden credential salt")
	}
	hash, err := base64.RawURLEncoding.DecodeString(parts[3])
	if err != nil || len(hash) != 32 {
		return nil, nil, errors.New("invalid hidden credential digest")
	}
	return salt, hash, nil
}

func hashHiddenPasscode(passcode string) (string, error) {
	salt := make([]byte, 16)
	if _, err := rand.Read(salt); err != nil {
		return "", fmt.Errorf("creating hidden passcode salt: %w", err)
	}
	hash := argon2.IDKey(norm.NFC.Bytes([]byte(passcode)), salt, hiddenArgonTime, hiddenArgonMemory, hiddenArgonThreads, 32)
	return "argon2id$" + hiddenArgonParameters + "$" + base64.RawURLEncoding.EncodeToString(salt) + "$" + base64.RawURLEncoding.EncodeToString(hash), nil
}

func hiddenTokenDigest(token string) string {
	if len(token) != 43 {
		return ""
	}
	bytes, err := base64.RawURLEncoding.DecodeString(token)
	if err != nil || len(bytes) != 32 {
		return ""
	}
	digest := sha256.Sum256(bytes)
	return hex.EncodeToString(digest[:])
}

func (s *Store) hiddenNow() time.Time {
	if s.photoHiddenNow != nil {
		return s.photoHiddenNow().UTC()
	}
	return time.Now().UTC()
}

func (s *Store) hiddenSession(ctx context.Context, q metadataQuerier) (time.Time, error) {
	token, _ := ctx.Value(hiddenTokenKey{}).(string)
	digest := hiddenTokenDigest(token)
	if digest == "" {
		return time.Time{}, ErrHiddenLocked
	}
	s.photoHiddenSessionsMu.Lock()
	until := s.photoHiddenSessions[digest]
	if !until.After(s.hiddenNow()) {
		delete(s.photoHiddenSessions, digest)
		s.photoHiddenSessionsMu.Unlock()
		return time.Time{}, ErrHiddenLocked
	}
	s.photoHiddenSessionsMu.Unlock()
	var configured bool
	if err := q.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM photo_hidden_credentials WHERE singleton=1)`).Scan(&configured); err != nil {
		return time.Time{}, err
	}
	if !configured {
		return time.Time{}, ErrHiddenLocked
	}
	return until, nil
}

func (s *Store) PhotoHiddenState(ctx context.Context) (PhotoHiddenState, error) {
	var state PhotoHiddenState
	err := s.photoReadTx(ctx, func(tx *sql.Tx) error {
		if err := tx.QueryRowContext(ctx, `SELECT receipt_id FROM photo_change_receipts WHERE rowid IN (SELECT MAX(rowid) FROM photo_change_receipts WHERE operation='hide' UNION ALL SELECT MAX(rowid) FROM photo_change_receipts WHERE operation='unhide') ORDER BY rowid DESC LIMIT 1`).Scan(&state.ChangeID); err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM photo_hidden_credentials)`).Scan(&state.Configured); err != nil {
			return err
		}
		var until string
		err := tx.QueryRowContext(ctx, `SELECT locked_until FROM photo_hidden_lockout WHERE singleton=1`).Scan(&until)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		if err == nil {
			t, err := time.Parse(time.RFC3339Nano, until)
			if err != nil {
				return fmt.Errorf("reading hidden lockout expiry: %w", err)
			}
			if now := s.hiddenNow(); t.After(now) {
				state.LockedUntil = &until
				state.LockedFor = hiddenSecondsUntil(now, t)
			}
		}
		expiry, err := s.hiddenSession(ctx, tx)
		if err == nil {
			state.ExpiresAt = new(expiry.Format(timestampLayout))
			state.ExpiresIn = hiddenSecondsUntil(s.hiddenNow(), expiry)
		} else if !errors.Is(err, ErrHiddenLocked) {
			return err
		}
		return nil
	})
	return state, err
}

func clearHiddenAuthTx(ctx context.Context, tx *sql.Tx, credentials bool) error {
	tables := []string{"photo_hidden_failures", "photo_hidden_lockout"}
	if credentials {
		tables = append(tables, "photo_hidden_credentials")
	}
	for _, table := range tables {
		if _, err := tx.ExecContext(ctx, `DELETE FROM `+table); err != nil {
			return err
		}
	}
	return nil
}

// consumeHiddenPasscodeTx commits failures through the caller's denial result.
func (s *Store) consumeHiddenPasscodeTx(ctx context.Context, tx *sql.Tx, passcode string) (error, error) {
	now := s.hiddenNow()
	var until string
	err := tx.QueryRowContext(ctx, `SELECT locked_until FROM photo_hidden_lockout WHERE singleton=1`).Scan(&until)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return nil, err
	}
	if err == nil {
		t, err := time.Parse(time.RFC3339Nano, until)
		if err != nil {
			return nil, fmt.Errorf("reading hidden lockout expiry: %w", err)
		}
		if t.After(now) {
			return &HiddenLockoutError{Until: t}, nil
		}
	}
	var encoded string
	err = tx.QueryRowContext(ctx, `SELECT passcode_hash FROM photo_hidden_credentials WHERE singleton=1`).Scan(&encoded)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrHiddenNotConfigured, nil
	}
	if err != nil {
		return nil, err
	}
	salt, expected, err := hiddenHashParts(encoded)
	if err != nil {
		return nil, err
	}
	hash := argon2.IDKey(norm.NFC.Bytes([]byte(passcode)), salt, hiddenArgonTime, hiddenArgonMemory, hiddenArgonThreads, 32)
	if subtle.ConstantTimeCompare(hash, expected) == 1 {
		if err := clearHiddenAuthTx(ctx, tx, false); err != nil {
			return nil, err
		}
		return nil, nil //nolint:nilnil // The first error is a committed denial; the second aborts the transaction.
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM photo_hidden_failures WHERE occurred_at<?`, now.Add(-time.Minute).Format(timestampLayout)); err != nil {
		return nil, err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO photo_hidden_failures(occurred_at) VALUES(?)`, now.Format(timestampLayout)); err != nil {
		return nil, err
	}
	var count int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM photo_hidden_failures`).Scan(&count); err != nil {
		return nil, err
	}
	if count >= 5 {
		until := now.Add(5 * time.Minute)
		_, err := tx.ExecContext(ctx, `INSERT INTO photo_hidden_lockout(singleton,locked_until) VALUES(1,?) ON CONFLICT(singleton) DO UPDATE SET locked_until=excluded.locked_until`, until.Format(timestampLayout))
		return &HiddenLockoutError{Until: until}, err
	}
	return ErrHiddenPasscode, nil
}

func (s *Store) SetupPhotoHidden(ctx context.Context, passcode string) error {
	if err := validHiddenPasscode(passcode); err != nil {
		return err
	}
	// Hash before taking the vault's single write connection.
	hash, err := hashHiddenPasscode(passcode)
	if err != nil {
		return err
	}
	return s.withLogicalTx(ctx, func(tx *sql.Tx) error {
		var exists bool
		if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM photo_hidden_credentials)`).Scan(&exists); err != nil {
			return err
		}
		if exists {
			return ErrHiddenConfigured
		}
		_, err := tx.ExecContext(ctx, `INSERT INTO photo_hidden_credentials(singleton,passcode_hash) VALUES(1,?)`, hash)
		return err
	})
}

func (s *Store) UnlockPhotoHidden(ctx context.Context, passcode string) (string, time.Time, error) {
	s.photoHiddenAuthMu.Lock()
	defer s.photoHiddenAuthMu.Unlock()
	if err := validHiddenPasscode(passcode); err != nil {
		return "", time.Time{}, err
	}
	var token string
	var expiry time.Time
	var denial error
	err := s.withStorageTx(ctx, func(tx *sql.Tx) error {
		var err error
		denial, err = s.consumeHiddenPasscodeTx(ctx, tx, passcode)
		if err != nil || denial != nil {
			return err
		}
		bytes := make([]byte, 32)
		if _, err := rand.Read(bytes); err != nil {
			return fmt.Errorf("creating hidden session token: %w", err)
		}
		token = base64.RawURLEncoding.EncodeToString(bytes)
		expiry = s.hiddenNow().Add(5 * time.Minute)
		return nil
	})
	if err != nil {
		return "", time.Time{}, err
	}
	if denial == nil {
		s.photoHiddenSessionsMu.Lock()
		s.scanPhotoHiddenSessions()
		s.photoHiddenSessions[hiddenTokenDigest(token)] = expiry
		s.photoHiddenSessionsMu.Unlock()
	}
	return token, expiry, denial
}

func (s *Store) ChangePhotoHidden(ctx context.Context, old, next string) error {
	if err := validHiddenPasscode(next); err != nil {
		return err
	}
	// Hash before taking the vault's single write connection.
	hash, err := hashHiddenPasscode(next)
	if err != nil {
		return err
	}
	return s.editPhotoHidden(ctx, old, "change", hash)
}

func (s *Store) DisablePhotoHidden(ctx context.Context, passcode string) error {
	return s.editPhotoHidden(ctx, passcode, "disable", "")
}

func (s *Store) editPhotoHidden(ctx context.Context, passcode, operation, nextHash string) error {
	s.photoHiddenAuthMu.Lock()
	defer s.photoHiddenAuthMu.Unlock()
	if err := validHiddenPasscode(passcode); err != nil {
		return err
	}
	var denial error
	err := s.withLogicalTx(ctx, func(tx *sql.Tx) error {
		var err error
		denial, err = s.consumeHiddenPasscodeTx(ctx, tx, passcode)
		if err != nil || denial != nil {
			return err
		}
		if operation == "disable" {
			rows, err := tx.QueryContext(ctx, `SELECT asset_id FROM photo_assets WHERE hidden_at IS NOT NULL ORDER BY asset_id`)
			if err != nil {
				return err
			}
			defer func() { _ = rows.Close() }()
			var ids []string
			for rows.Next() {
				var id string
				if err := rows.Scan(&id); err != nil {
					return err
				}
				ids = append(ids, id)
			}
			if err := rows.Err(); err != nil {
				return err
			}
			if err := rows.Close(); err != nil {
				return err
			}
			for _, id := range ids {
				asset, err := photoAssetByIDQuery(ctx, tx, id)
				if err != nil {
					return err
				}
				next := asset
				next.HiddenAt = nil
				if _, err := commitPhotoAssetTx(ctx, tx, asset, next, "unhide"); err != nil {
					return err
				}
			}
			return clearHiddenAuthTx(ctx, tx, true)
		}
		if _, err := tx.ExecContext(ctx, `UPDATE photo_hidden_credentials SET passcode_hash=? WHERE singleton=1`, nextHash); err != nil {
			return err
		}
		return clearHiddenAuthTx(ctx, tx, false)
	})
	if err != nil {
		return err
	}
	if denial == nil {
		s.clearHiddenSessions()
	}
	return denial
}

func (s *Store) ResetPhotoHidden(ctx context.Context) error {
	s.photoHiddenAuthMu.Lock()
	defer s.photoHiddenAuthMu.Unlock()
	if err := s.withLogicalTx(ctx, func(tx *sql.Tx) error { return clearHiddenAuthTx(ctx, tx, true) }); err != nil {
		return err
	}
	s.clearHiddenSessions()
	return nil
}

func (s *Store) LockPhotoHidden(ctx context.Context) error {
	s.photoHiddenAuthMu.Lock()
	defer s.photoHiddenAuthMu.Unlock()
	s.clearHiddenSessions()
	return nil
}

func (s *Store) clearHiddenSessions() {
	s.photoHiddenSessionsMu.Lock()
	clear(s.photoHiddenSessions)
	s.photoHiddenSessionsMu.Unlock()
	s.publishPhotoChanges([]PhotoEvent{{Refetch: true}})
}

func (s *Store) SetPhotoAssetHidden(ctx context.Context, id string, revision int64, hidden bool) (PhotoAsset, error) {
	operation := "hide"
	if !hidden {
		operation = "unhide"
	}
	return s.mutatePhotoAsset(ctx, id, revision, operation, func(tx *sql.Tx, asset *PhotoAsset) (bool, error) {
		if (asset.HiddenAt != nil) == hidden {
			return false, nil
		}
		if hidden {
			var exists bool
			if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM photo_hidden_credentials)`).Scan(&exists); err != nil {
				return false, err
			}
			if !exists {
				return false, ErrHiddenNotConfigured
			}
		}
		asset.HiddenAt = nil
		if hidden {
			asset.HiddenAt = new(s.hiddenNow().Format(timestampLayout))
		}
		return true, nil
	})
}

func photoVisibilityPredicate(hidden bool) string {
	if hidden {
		return `a.hidden_at IS NOT NULL`
	}
	return `a.hidden_at IS NULL`
}
