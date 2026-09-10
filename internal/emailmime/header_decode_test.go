package emailmime

import (
	"mime"
	"net/mail"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/net/html/charset"
)

func TestDecodeHeaderBoundedMatchesMIMEWordDecoder(t *testing.T) {
	reference := &mime.WordDecoder{CharsetReader: charset.NewReaderLabel}
	for _, value := range []string{
		"plain text",
		"before =?utf-8?B?YWJj?= after",
		"=?utf-8?Q?one?= \t =?utf-8?Q?two?=",
		"=?iso-8859-1?Q?caf=E9?=",
		"=?us-ascii?B?gA==?=",
		"=?windows-1252?Q?=80?=",
		"broken =?utf-8?B?%%%?= word",
	} {
		t.Run(value, func(t *testing.T) {
			want, wantErr := reference.DecodeHeader(value)
			got, gotErr := decodeHeaderBounded(value, int64(len(want)))
			if wantErr != nil {
				require.Error(t, gotErr)
				return
			}
			require.NoError(t, gotErr)
			assert.Equal(t, want, got)
			_, gotErr = decodeHeaderBounded(value, int64(len(want)-1))
			require.ErrorIs(t, gotErr, errDisplayBudget)
		})
	}
}

func TestProjectAddressesBoundedMatchesMailProjection(t *testing.T) {
	for _, value := range []string{
		"A <a@example.test>, b@example.test",
		`"Last, First" <person@example.test>`,
		"Friends: A <a@example.test>, b@example.test;",
		"Friends: A <a@example.test>; (trailing comment)",
		"undisclosed-recipients:;",
		"user@[IPv6:2001:db8::1]",
	} {
		t.Run(value, func(t *testing.T) {
			want, err := mail.ParseAddressList(value)
			require.NoError(t, err)
			got, _, err := projectAddressesBounded(value, 1<<20)
			require.NoError(t, err)
			require.Len(t, got, len(want))
			for index := range want {
				assert.Equal(t, want[index].Name, got[index].Name)
				assert.Equal(t, want[index].Address, got[index].Address)
			}
		})
	}
}
