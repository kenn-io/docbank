package store

import (
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"strings"
	"unicode/utf8"
)

const maxNoteRawBytes = 24 << 20

// ErrNoteRawLimit is a terminal projection limit, independent of parser limits.
var ErrNoteRawLimit = errors.New("note frontmatter JSON exceeds 24 MiB")

type noteRawWriter struct {
	ctx context.Context
	strings.Builder
}

func (w *noteRawWriter) Write(p []byte) (int, error) {
	if err := w.ctx.Err(); err != nil {
		return 0, err
	}
	if len(p) > maxNoteRawBytes-w.Len() {
		return 0, ErrNoteRawLimit
	}
	return w.Builder.Write(p)
}

// encodeNoteRaw uses one standard JSON traversal. String admission also covers
// map keys and unflushed nested prefixes, which a limiting Writer alone misses.
// Counting quotes uses 6,146 bytes of scratch; an admitted token is <=24 MiB. The
// destination payload is <=24 MiB; its backing allocation and encoder/namespace
// storage are O(cap + depth).
// Encoding work is O(cap): rejected strings stop after their remaining budget;
// admitted strings are quoted twice. No traversal of the whole Raw precedes it.
func encodeNoteRaw(ctx context.Context, raw map[string]any) (string, error) {
	w := &noteRawWriter{ctx: ctx}
	marshalStrings := json.MarshalToFunc(func(enc *jsontext.Encoder, value string) error {
		_, count := enc.StackIndex(enc.StackDepth())
		delimiter := int64(0)
		if count > 0 {
			delimiter = 1 // comma or colon; compact JSON has no other whitespace.
		}
		remaining := int64(maxNoteRawBytes) - enc.OutputOffset() - delimiter
		quotedSize := int64(2)
		var scratch [6*1024 + 2]byte
		for rest := value; rest != ""; {
			if err := ctx.Err(); err != nil {
				return err
			}
			n := min(len(rest), 1024)
			for n > 0 && n < len(rest) && !utf8.RuneStart(rest[n]) {
				n--
			}
			if n == 0 {
				return errors.New("invalid UTF-8 in note frontmatter")
			}
			quoted, err := jsontext.AppendQuote(scratch[:0], rest[:n])
			if err != nil {
				return err
			}
			quotedSize += int64(len(quoted) - 2)
			if quotedSize > remaining {
				return ErrNoteRawLimit
			}
			rest = rest[n:]
		}
		if quotedSize > remaining {
			return ErrNoteRawLimit
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		quoted, err := jsontext.AppendQuote(make([]byte, 0, int(quotedSize)), value)
		if err != nil {
			return err
		}
		return enc.WriteValue(quoted)
	})
	// MarshalWrite omits the top-level newline, making the cap exact.
	if err := json.MarshalWrite(w, raw, json.WithMarshalers(marshalStrings)); err != nil {
		return "", err
	}
	return w.String(), nil
}
