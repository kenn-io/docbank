package emailmime

import (
	"fmt"
	"io"
	"net/mail"
	"runtime"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/docbank/document"
)

func TestAddressRepeatedEncodedWordsRefuseBeforeAllocating(t *testing.T) {
	for _, count := range []int{32, 8192} {
		for _, word := range []string{"=?utf-8?B?YQ==?=", "=?utf-8?Q?a?=", "=?iso-8859-1?Q?=E9?="} {
			value := strings.Repeat(word+" ", count) + "<a@example.test>"
			for _, budget := range []int64{0, 7} {
				t.Run(fmt.Sprintf("%s/words=%d/budget=%d", word, count, budget), func(t *testing.T) {
					runtime.GC()
					var before, after runtime.MemStats
					runtime.ReadMemStats(&before)
					got, _, err := projectAddressesBounded(value, budget)
					runtime.ReadMemStats(&after)
					require.ErrorIs(t, err, errDisplayBudget)
					assert.Nil(t, got)
					allocated := after.TotalAlloc - before.TotalAlloc
					t.Logf("refusal allocated %d bytes", allocated)
					assert.Less(t, allocated, uint64(128<<10), "repeated-word preflight allocated before capacity refusal")
				})
			}
		}
	}
}

func TestAddressRepeatedEncodedWordsActualCapacity(t *testing.T) {
	const address = "a@example.test"
	for _, tc := range []struct{ word, name string }{
		{"=?utf-8?B?YQ==?=", "a"},
		{"=?utf-8?Q?=C3=A9?=", "é"},
		{"=?iso-8859-1?Q?=E9?=", "é"},
		{"=?us-ascii?Q?=FF?=", "�"},
	} {
		t.Run(tc.word, func(t *testing.T) {
			const count = 8192
			value := strings.Repeat(tc.word+" ", count) + "<" + address + ">"
			name := strings.Repeat(tc.name, count)
			capacity := int64(len(name) + len(address))
			for _, delta := range []int64{-1, 0, 1} {
				got, cost, err := projectAddressesBounded(value, capacity+delta)
				if delta < 0 {
					require.ErrorIs(t, err, errDisplayBudget)
					assert.Nil(t, got)
					continue
				}
				require.NoError(t, err)
				assert.Equal(t, []document.EmailAddressV1{{Name: name, Address: address}}, got)
				assert.Equal(t, capacity, cost)
			}
		})
	}
}

func TestAddressQuotedPairEncodedCommentActualCapacity(t *testing.T) {
	for _, word := range []string{`\=?utf-8?B?YWJj?=`, `\=\?utf-8\?B\?YWJj\?=`, `\=\?utf-8\?Q\?=C3=A9\?=`, `\=\?iso-8859-1\?Q\?=E9\?=`} {
		value := "a@example.test (" + word + ")"
		reference, err := mail.ParseAddress(value)
		require.NoError(t, err)
		capacity := int64(len(reference.Name) + len(reference.Address))
		for _, delta := range []int64{-1, 0, 1} {
			t.Run(fmt.Sprintf("%s/delta=%d", word, delta), func(t *testing.T) {
				got, cost, err := projectAddressesBounded(value, capacity+delta)
				if delta < 0 {
					require.ErrorIs(t, err, errDisplayBudget)
					assert.Nil(t, got)
					return
				}
				require.NoError(t, err)
				assert.Equal(t, []document.EmailAddressV1{{Name: reference.Name, Address: reference.Address}}, got)
				assert.Equal(t, capacity, cost)
			})
		}
	}
}

func TestAddressQuotedPairEncodedCommentPublicCapacity(t *testing.T) {
	const limit = 1 << 20
	const from = `a@example.test (\=\?utf-8\?B\?YWJj\?=)`
	const address = "a@example.test"
	capacity := len(from) + len(address) + 3
	for _, delta := range []int{-1, 0, 1} {
		t.Run(strconv.Itoa(delta), func(t *testing.T) {
			headers := "Subject: " + strings.Repeat("b", limit/2-capacity-delta) + "\r\nFrom: " + from + "\r\n\r\n"
			raw := "Subject: " + strings.Repeat("a", limit/2) + "\r\nContent-Type: multipart/mixed; boundary=outer\r\n\r\n" +
				"--outer\r\nContent-Type: message/rfc822\r\n\r\n" + headers + "body\r\n--outer--\r\n"
			result := decodePublicFixture(t, []byte(raw))
			field := result.Evidence.Inventory.Messages[1].Fields.From[0]
			assert.Equal(t, 1, field.HeaderIndex)
			if delta < 0 {
				assert.Equal(t, document.EmailInterpretationUnsupported, field.State)
				assert.Nil(t, field.Text)
				assert.Nil(t, field.Addresses)
			} else {
				assert.Equal(t, document.EmailInterpretationDecoded, field.State)
				require.NotNil(t, field.Text)
				assert.Equal(t, from, *field.Text)
				require.NotNil(t, field.Addresses)
				assert.Equal(t, []document.EmailAddressV1{{Name: "abc", Address: address}}, *field.Addresses)
			}
			stream, err := result.OpenArtifact(t.Context(), "1.1.1", "raw_headers")
			require.NoError(t, err)
			got, err := io.ReadAll(stream)
			require.NoError(t, err)
			require.NoError(t, stream.Close())
			assert.Equal(t, headers, string(got))
		})
	}
}

func TestAddressCommentCompositionRefusesBeforeAllocating(t *testing.T) {
	for _, count := range []int{32, 8192} {
		for name, comment := range map[string]string{
			"repeated words": strings.Repeat(`\=\?utf-8\?B\?YQ==\?= `, count),
			"one word":       `\=\?utf-8\?B\?` + strings.Repeat(`\Y\W\J\j`, count) + `\?=`,
		} {
			value := "a@example.test (" + comment + ")"
			for _, remaining := range []int64{0, 7} {
				t.Run(fmt.Sprintf("%s/count=%d/remaining=%d", name, count, remaining), func(t *testing.T) {
					runtime.GC()
					var before, after runtime.MemStats
					runtime.ReadMemStats(&before)
					got, _, err := projectAddressesBounded(value, 14+remaining)
					runtime.ReadMemStats(&after)
					require.ErrorIs(t, err, errDisplayBudget)
					assert.Nil(t, got)
					allocated := after.TotalAlloc - before.TotalAlloc
					t.Logf("comment refusal allocated %d bytes", allocated)
					assert.Less(t, allocated, uint64(128<<10), "unescape/decode retained an intermediate before refusing")
				})
			}
		}
	}
}

func TestAddressDiscardedSyntaxDoesNotConsumeDisplayCapacity(t *testing.T) {
	const address = "a@example.test"
	for _, name := range []string{
		`"` + strings.Repeat(`\a`, 512<<10) + `"`,
		strings.Repeat("=?utf-8?B?YQ==?= ", 8192),
		"Group " + strings.Repeat("=?utf-8?Q??= ", 8192),
	} {
		for _, member := range []string{"", address} {
			value := name + ":" + member + "; (" + strings.Repeat(`\a`, 512<<10) + ")"
			got, cost, err := projectAddressesBounded(value, int64(len(member)))
			require.NoError(t, err)
			assert.Equal(t, int64(len(member)), cost)
			if member == "" {
				assert.Empty(t, got)
			} else {
				assert.Equal(t, []document.EmailAddressV1{{Address: address}}, got)
			}
		}
	}
}

func TestAddressEncodedWordCursorPreservesGrammar(t *testing.T) {
	for _, word := range []string{
		"=?utf-8?B?YQ==?=", "=?UTF-8?b?YWJj?=", "=?utf-8?Q?a_b?=",
		"=?iso-8859-1?q?=E9?=", "=?us-ascii?B?/w==?=", "=?utf-8?Q?=FF?=",
		"=?utf-8?B?YR==?=", "=?utf-8?B?YWK=?=", // Nonzero unused bits remain tolerated.
		"=?utf-8?B?YQ?=", "=?utf-8?B?YQ==Yg==?=", "=?utf-8?B?YWJj!?=",
		"=?utf-8?Q?=ZZ?=", "=?utf-8?Q?abc=?=", "=?utf-8?Q?a?b?=", "=?utf-8?X?abc?=",
		"=?unknown?B?YWJj?=", "=?unknown?B?bad!?=", "=?windows-1252?Q?name?=",
		"=?" + strings.Repeat("x", 1024) + "?Q?abc?=",
	} {
		escaped := strings.NewReplacer("=", `\=`, "?", `\?`).Replace(word)
		for _, value := range []string{word + " <a@example.test>", "a@example.test (" + word + ")", "a@example.test (" + escaped + ")", word + ":;"} {
			want, wantErr := mail.ParseAddressList(value)
			got, _, gotErr := projectAddressesBounded(value, 1<<20)
			require.Equal(t, wantErr == nil, gotErr == nil, "input %q: reference=%v; bounded=%v", value, wantErr, gotErr)
			if wantErr != nil {
				continue
			}
			require.Len(t, got, len(want), "input %q", value)
			for i := range want {
				assert.Equal(t, want[i].Name, got[i].Name, "input %q", value)
				assert.Equal(t, want[i].Address, got[i].Address, "input %q", value)
			}
		}
	}
}

func TestAddressEncodedCharsetComparisonPreservesCaseFolding(t *testing.T) {
	for _, word := range []string{"=?uſ-aſcii?Q?a?=", `\=\?uſ-aſcii\?Q\?a\?=`} {
		got, cost, err := projectAddressesBounded("a@example.test ("+word+")", 15)
		require.NoError(t, err)
		assert.Equal(t, []document.EmailAddressV1{{Name: "a", Address: "a@example.test"}}, got)
		assert.Equal(t, int64(15), cost)
	}
}
