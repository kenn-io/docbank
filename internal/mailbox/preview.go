package mailbox

import (
	"context"
	"errors"
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
	remaining := int64(16 << 20)
	for _, entry := range entries {
		if entry.Size == 0 {
			continue
		}
		stream, err := openEntry(ctx, entry, r, 0)
		if err != nil {
			return out, err
		}
		limited := &io.LimitedReader{R: contextReader{ctx, stream}, N: remaining}
		scanner, err := NewScanner(limited, dialect, s.Spool, 128<<20)
		if err != nil {
			return out, errors.Join(err, stream.Close())
		}
		for {
			m, err := scanner.Next(ctx)
			if limited.N == 0 {
				out.HasMore = true
				if m != nil {
					err = errors.Join(err, m.Close())
				}
				return out, errors.Join(err, stream.Close())
			}
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
		remaining = limited.N
		if err = stream.Close(); err != nil {
			return out, err
		}
	}
	return out, nil
}
