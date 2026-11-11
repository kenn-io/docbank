package notemarkdown

import (
	"context"
	"crypto/sha256"
)

// Fixed-size digest keys keep map hashing bounded; collisions are compared in
// cancellable chunks rather than assumed to be identical strings.
type stringSet map[[32]byte][]string

func (s stringSet) add(ctx context.Context, value string, b *budget) (bool, error) {
	digest, err := textDigest(ctx, value, b)
	if err != nil {
		return false, err
	}
	for _, old := range s[digest] {
		if len(old) != len(value) {
			continue
		}
		same := true
		for i := 0; i < len(value); i += 1024 {
			end := min(i+1024, len(value))
			if err := b.step(ctx, end-i); err != nil {
				return false, err
			}
			if old[i:end] != value[i:end] {
				same = false
				break
			}
		}
		if same {
			return true, nil
		}
	}
	s[digest] = append(s[digest], value)
	return false, nil
}

func textDigest(ctx context.Context, value string, b *budget) ([32]byte, error) {
	h := sha256.New()
	for i := 0; i < len(value); i += 1024 {
		end := min(i+1024, len(value))
		if err := b.step(ctx, end-i); err != nil {
			return [32]byte{}, err
		}
		_, _ = h.Write([]byte(value[i:end]))
	}
	var digest [32]byte
	h.Sum(digest[:0])
	return digest, nil
}

type stringCounter struct {
	key   string
	value int
}
type stringCounters map[[32]byte][]*stringCounter

func (s stringCounters) counter(ctx context.Context, key string, b *budget) (*int, error) {
	digest, err := textDigest(ctx, key, b)
	if err != nil {
		return nil, err
	}
	for _, entry := range s[digest] {
		if len(entry.key) != len(key) {
			continue
		}
		equal := true
		for i := 0; i < len(key); i += 1024 {
			end := min(i+1024, len(key))
			if err := b.step(ctx, end-i); err != nil {
				return nil, err
			}
			if entry.key[i:end] != key[i:end] {
				equal = false
				break
			}
		}
		if equal {
			return &entry.value, nil
		}
	}
	entry := &stringCounter{key: key}
	s[digest] = append(s[digest], entry)
	return &entry.value, nil
}
