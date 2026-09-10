package emailmime

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"net/mail"
	"runtime"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document"
)

func TestDecodeLongTransportPaddingKeepsEveryPart(t *testing.T) {
	for _, position := range []int{0, 1, 2} {
		for _, lineBytes := range []int{4095, 4096, 4097, 5005, 128 << 10} {
			for _, space := range []string{" ", "\t"} {
				t.Run(fmt.Sprintf("position%d/bytes%d/pad%x", position, lineBytes, space), func(t *testing.T) {
					lines := []string{"--b\r\n", "--b\r\n", "--b--\r\n"}
					prefix := "--b"
					if position == 2 {
						prefix += "--"
					}
					lines[position] = prefix + strings.Repeat(space, lineBytes-len(prefix)-2) + "\r\n"
					header := "Content-Type: text/plain; charset=utf-8\r\nX-Part: first\r\n\r\n"
					second := "Content-Type: application/octet-stream\r\nX-Part: second\r\n\r\n"
					result := decodeFixture(t, "Content-Type: multipart/mixed; boundary=b\r\n\r\n"+lines[0]+header+"first payload\r\n"+lines[1]+second+"second payload\r\n"+lines[2])
					require.Equal(t, document.EmailInventoryComplete, result.Evidence.Inventory.State)
					require.Len(t, result.Evidence.Inventory.Parts, 3)
					for i, want := range []struct{ path, headers, body string }{{"1.1", header, "first payload"}, {"1.2", second, "second payload"}} {
						require.Equal(t, want.path, result.Evidence.Inventory.Parts[i+1].Path)
						for role, expected := range map[string]string{"raw_headers": want.headers, "decoded_payload": want.body} {
							r, err := result.OpenArtifact(t.Context(), want.path, role)
							require.NoError(t, err)
							got, err := io.ReadAll(r)
							require.NoError(t, err)
							require.NoError(t, r.Close())
							require.Equal(t, expected, string(got))
						}
					}
				})
			}
		}
	}
}

func TestMultipartTransportPaddingStreamsWithBoundedAllocation(t *testing.T) {
	raw := "--b" + strings.Repeat(" \t", 4<<20) + "\r\n\r\nbody\r\n--b--" + strings.Repeat("\t ", 4<<20)
	runtime.GC()
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	var hb int64
	var hf int
	reader := newMultipartReader(strings.NewReader(raw), "b", func(r *bufio.Reader) (parsedHeaderBlock, error) {
		return readHeaderBlock(context.Background(), r, 1024, 10, &hb, 2048, &hf, 20)
	})
	part, err := reader.NextPart()
	require.NoError(t, err)
	got, err := io.ReadAll(part)
	require.NoError(t, err)
	require.Equal(t, "body", string(got))
	_, err = reader.NextPart()
	require.ErrorIs(t, err, io.EOF)
	runtime.ReadMemStats(&after)
	require.Less(t, after.TotalAlloc-before.TotalAlloc, uint64(128<<10), "transport padding must not accumulate a source-sized line")
}

func TestMalformedLongDelimiterRemainsCanonicalPartial(t *testing.T) {
	result := decodeFixture(t, "Content-Type: multipart/mixed; boundary=b\r\n\r\n--b\r\n\r\nfirst\r\n--b"+strings.Repeat(" ", 8192)+"invalid\r\n")
	require.Equal(t, document.EmailInventoryPartial, result.Evidence.Inventory.State)
	require.NotNil(t, result.Evidence.Inventory.Termination)
	require.Equal(t, document.EmailDiagnosticCode("boundary_invalid"), result.Evidence.Inventory.Termination.Code)
}

func TestEmptyEncodedPhraseWordsMatchMailAndPreserveRawEvidence(t *testing.T) {
	for _, empty := range []string{"=?utf-8?B??=", "=?utf-8?Q??="} {
		for _, tc := range []struct {
			phrase, want string
			invalid      bool
		}{
			{empty + " <0@0>", "", true}, {"x " + empty + " <0@0>", "x", false}, {"a " + empty + " b <0@0>", "a b", false},
			{empty + " a <0@0>", "a", false}, {empty + " =?utf-8?Q?a?= <0@0>", "a", false}, {"=?utf-8?Q?a?= " + empty + " =?utf-8?Q?b?= <0@0>", "ab", false},
			{empty + ":;", "", true}, {"Group " + empty + ":0@0;", "", false}, {`"" <0@0>`, "", false}, {`"" ` + empty + ` <0@0>`, "", false},
		} {
			t.Run(tc.phrase, func(t *testing.T) {
				reference, refErr := mail.ParseAddressList(tc.phrase)
				got, _, err := projectAddressesBounded(tc.phrase, 1024)
				if tc.invalid {
					require.Error(t, refErr)
					require.Error(t, err)
				} else {
					require.NoError(t, refErr)
					require.NoError(t, err)
					require.Len(t, got, 1)
					require.Equal(t, tc.want, got[0].Name)
					require.Equal(t, reference[0].Name, got[0].Name)
				}
				raw := "From: " + tc.phrase + "\r\nContent-Type: text/plain; charset=utf-8\r\n\r\n"
				decoded := decodeFixture(t, raw+"body")
				field := decoded.Evidence.Inventory.Messages[0].Fields.From[0]
				if tc.invalid {
					require.Equal(t, document.EmailInterpretationInvalid, field.State)
					require.Nil(t, field.Addresses)
				} else {
					require.Equal(t, document.EmailInterpretationDecoded, field.State)
					require.NotNil(t, field.Addresses)
					require.Equal(t, tc.want, (*field.Addresses)[0].Name)
				}
				r, err := decoded.OpenArtifact(t.Context(), "1", "raw_headers")
				require.NoError(t, err)
				b, err := io.ReadAll(r)
				require.NoError(t, err)
				require.NoError(t, r.Close())
				require.True(t, bytes.Equal([]byte(raw), b))
			})
		}
	}
}
