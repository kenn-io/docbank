package notemarkdown

import (
	"context"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// These enter later phases directly, so input admission cannot satisfy their
// cancellation assertions. No timers or scheduler races are involved.
func TestLaterScanCancellation(t *testing.T) {
	text := strings.Repeat("a", 30000)
	cases := map[string]func(context.Context, *budget) error{
		"line": func(ctx context.Context, b *budget) error {
			_, _, err := sourceLineAt(ctx, []byte(text), 0, b)
			return err
		},
		"normalization": func(ctx context.Context, b *budget) error { _, err := headingKey(ctx, text, b); return err },
		"tag": func(ctx context.Context, b *budget) error {
			_, err := htmlTagEnd(ctx, []byte("<x a='"+text+"'>"), 0, len(text)+8, b)
			return err
		},
		"tag-validation": func(ctx context.Context, b *budget) error {
			r := htmlRuneReader{ctx: ctx, b: b, src: []byte("<x a='" + text + "'>")}
			openingHTMLTag.MatchReader(&r)
			return r.err
		},
		"html-close": func(ctx context.Context, b *budget) error {
			_, err := htmlEndMatches(ctx, []byte(text), "</script>", b)
			return err
		},
		"delimiter": func(ctx context.Context, b *budget) error {
			_, err := byteRun(ctx, []byte(strings.Repeat("`", 30000)), '`', b)
			return err
		},
		"indent": func(ctx context.Context, b *budget) error {
			_, _, err := leadingIndent(ctx, []byte(strings.Repeat(" ", 30000)), 0, b)
			return err
		},
		"autolink-uri": func(ctx context.Context, b *budget) error {
			_, err := validAutolinkURI(ctx, []byte("https:"+text), b)
			return err
		},
		"autolink-email": func(ctx context.Context, b *budget) error {
			_, err := validAutolinkEmail(ctx, []byte(text+"@example.invalid"), b)
			return err
		},
		"heading-display": func(ctx context.Context, b *budget) error {
			_, err := headingText(ctx, []byte(text), Span{0, len(text)}, nil, b)
			return err
		},
		"code-index": func(ctx context.Context, b *budget) error {
			s := linkScanner{ctx: ctx, src: []byte(strings.Repeat("`", 30000)), b: b}
			return s.inline(0, len(s.src))
		},
	}
	for name, scan := range cases {
		t.Run(name, func(t *testing.T) {
			b := &budget{}
			ctx := &cancellationChecks{Context: t.Context(), remaining: 2}
			require.ErrorIs(t, scan(ctx, b), context.Canceled)
			require.LessOrEqual(t, b.work, 2048, "bulk admission cannot substitute for checks during scanning")
		})
	}
}
