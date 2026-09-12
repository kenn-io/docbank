package mailbox

import (
	"context"
	"errors"
	"go.kenn.io/docbank/internal/store"
	"io"
)

type Preview struct {
	EntryCount int       `json:"entry_count"`
	Dialect    string    `json:"dialect"`
	Entries    []Entry   `json:"entries"`
	Samples    []Message `json:"samples"`
	HasMore    bool      `json:"has_more"`
}

func (s *Service) Preview(ctx context.Context, owner, id, dialect string) (_ Preview, retErr error) {
	if dialect == "" {
		dialect = "mboxrd"
	}
	out := Preview{Dialect: dialect, Samples: []Message{}}
	c, err := s.Store.MailboxContainer(ctx, owner, id)
	if err != nil {
		return out, err
	}
	entries, r, err := s.sourceEntries(ctx, c)
	if err != nil {
		return out, err
	}
	out.EntryCount = len(entries)
	out.Entries = entries[:min(len(entries), 100)]
	for _, entry := range entries {
		stream, err := openEntry(ctx, entry, r)
		if err != nil {
			return out, err
		}
		scanner, err := NewScanner(contextReader{ctx, stream}, dialect, s.Spool, 128<<20)
		if err != nil {
			return out, errors.Join(err, stream.Close())
		}
		for {
			m, err := scanner.Next(ctx)
			if errors.Is(err, io.EOF) {
				break
			}
			if err != nil {
				return out, errors.Join(err, stream.Close())
			}
			if closeErr := m.Close(); closeErr != nil {
				return out, errors.Join(closeErr, stream.Close())
			}
			if len(out.Samples) == 3 {
				out.HasMore = true
				return out, stream.Close()
			}
			out.Samples = append(out.Samples, *m)
		}
		if err = stream.Close(); err != nil {
			return out, err
		}
	}
	if len(out.Samples) == 0 {
		return out, store.ErrMailboxInvalid
	}
	return out, nil
}
