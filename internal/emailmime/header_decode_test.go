package emailmime

import (
	"encoding/base64"
	"fmt"
	"io"
	"net/mail"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/docbank/document"
)

func TestDecodeHeaderBoundsDecodedBytes(t *testing.T) {
	const encoded = "=?windows-1252?Q?=80?="
	decoded, err := decodeHeaderBounded(encoded, int64(len("€")))
	require.NoError(t, err)
	assert.Equal(t, "€", decoded)
	decoded, err = decodeHeaderBounded(encoded, int64(len("€")-1))
	require.ErrorIs(t, err, errDisplayBudget)
	assert.Empty(t, decoded)
}

func TestAddressProjectionBoundsCombinedDecodedBytes(t *testing.T) {
	const encoded = "=?windows-1252?Q?=80?= <a@example.test>, b@example.test"
	const cost = int64(len("€") + len("a@example.test") + len("b@example.test"))
	addresses, gotCost, err := projectAddressesBounded(encoded, cost)
	require.NoError(t, err)
	assert.Equal(t, []document.EmailAddressV1{{Name: "€", Address: "a@example.test"}, {Address: "b@example.test"}}, addresses)
	assert.Equal(t, cost, gotCost)
	addresses, gotCost, err = projectAddressesBounded(encoded, cost-1)
	require.ErrorIs(t, err, errDisplayBudget)
	assert.Nil(t, addresses)
	assert.Zero(t, gotCost)
}

func TestHeaderParsersRejectInputPastRecipeLimit(t *testing.T) {
	input := strings.Repeat("a", int(Recipe().Limits.HeaderBytes)+1)
	_, err := decodeHeaderBounded(input, int64(len(input)))
	require.ErrorIs(t, err, errDisplayBudget)
	_, _, err = projectAddressesBounded(input+" <a@example.test>", int64(len(input))+100)
	require.ErrorIs(t, err, errDisplayBudget)
}

func TestAddressProjectionRejectsInvalidDecodedUTF8(t *testing.T) {
	_, _, err := projectAddressesBounded("=?utf-8?Q?=FF?= <a@example.test>", 100)
	require.Error(t, err)
}

func TestDecodedHeaderBudgetUsesActualOutput(t *testing.T) {
	for _, test := range []struct {
		name, encoded, decoded string
		budget                 int64
		state                  document.EmailInterpretationState
	}{
		{"compressed with eight-byte budget", "=?utf-8?B?YWJj?=", "abc", 8, document.EmailInterpretationDecoded},
		{"compressed at capacity", "=?utf-8?B?YWJj?=", "abc", 3, document.EmailInterpretationDecoded},
		{"compressed below capacity", "=?utf-8?B?YWJj?=", "", 2, document.EmailInterpretationUnsupported},
		{"unicode at capacity", "=?utf-8?B?w6k=?=", "é", 2, document.EmailInterpretationDecoded},
		{"unicode below capacity", "=?utf-8?B?w6k=?=", "", 1, document.EmailInterpretationUnsupported},
	} {
		t.Run(test.name, func(t *testing.T) {
			d := &decoder{limits: Recipe().Limits}
			d.limits.HeaderDisplayBytes = test.budget
			message := d.interpretMessage("1", parsedHeaderBlock{fields: []parsedHeader{{index: 0, name: "subject", value: test.encoded, valid: true}}})
			field := message.Fields.Subject[0]
			assert.Equal(t, test.state, field.State)
			if test.state == document.EmailInterpretationDecoded {
				require.NotNil(t, field.Text)
				assert.Equal(t, test.decoded, *field.Text)
			}
		})
	}
}

func TestAddressProjectionBudgetUsesActualOutput(t *testing.T) {
	const value = "José <j@example.test>, A <a@example.test>"
	const projection = "José" + "j@example.test" + "A" + "a@example.test"
	exact := int64(len(value) + len(projection))
	for _, delta := range []int64{-1, 0, 1} {
		d := &decoder{limits: Recipe().Limits}
		d.limits.HeaderDisplayBytes = exact + delta
		message := d.interpretMessage("1", parsedHeaderBlock{fields: []parsedHeader{{index: 0, name: "from", value: value, valid: true}}})
		field := message.Fields.From[0]
		if delta < 0 {
			assert.Equal(t, document.EmailInterpretationUnsupported, field.State)
			assert.Nil(t, field.Text)
			assert.Nil(t, field.Addresses)
			continue
		}
		assert.Equal(t, document.EmailInterpretationDecoded, field.State)
		require.NotNil(t, field.Addresses)
		assert.Len(t, *field.Addresses, 2)
		assert.Equal(t, exact, d.headerDisplayBytes)
	}
}

func TestAddressProjectionPreservesActualLargeMailboxCapacity(t *testing.T) {
	const nameBytes = 128 << 10
	const address = "a@example.test"
	mailboxName := strings.Repeat("a", nameBytes)
	for name, value := range map[string]string{
		"encoded word": "=?utf-8?B?" + base64.StdEncoding.EncodeToString([]byte(mailboxName)) + "?= <" + address + ">",
		"quoted":       `"` + mailboxName + `" <` + address + `>`,
		"quoted pairs": `"` + strings.Repeat(`\a`, nameBytes) + `" <` + address + `>`,
	} {
		t.Run(name, func(t *testing.T) {
			capacity := int64(nameBytes + len(address))
			for _, delta := range []int64{-1, 0, 1} {
				addresses, cost, err := projectAddressesBounded(value, capacity+delta)
				if delta < 0 {
					require.ErrorIs(t, err, errDisplayBudget)
					assert.Nil(t, addresses)
					continue
				}
				require.NoError(t, err)
				require.Len(t, addresses, 1)
				assert.Len(t, addresses[0].Name, nameBytes)
				assert.Equal(t, address, addresses[0].Address)
				assert.Equal(t, capacity, cost)
			}
		})
	}
}

func TestEncodedAddressDisplayUsesActualProjectionCapacity(t *testing.T) {
	const nameBytes = 64 << 10
	const address = "a@example.test"
	name := strings.Repeat("a", nameBytes)
	encoded := "=?utf-8?B?" + base64.StdEncoding.EncodeToString([]byte(name)) + "?= <" + address + ">"
	decoded := name + " <" + address + ">"
	capacity := int64(len(decoded) + len(name) + len(address))
	for _, delta := range []int64{-1, 0, 1} {
		d := &decoder{limits: Recipe().Limits}
		d.limits.HeaderDisplayBytes = capacity + delta
		message := d.interpretMessage("1", parsedHeaderBlock{fields: []parsedHeader{{index: 0, name: "from", value: encoded, valid: true}}})
		field := message.Fields.From[0]
		if delta < 0 {
			assert.Equal(t, document.EmailInterpretationUnsupported, field.State)
			assert.Nil(t, field.Addresses)
			continue
		}
		assert.Equal(t, document.EmailInterpretationDecoded, field.State)
		require.NotNil(t, field.Text)
		assert.Equal(t, decoded, *field.Text)
		require.NotNil(t, field.Addresses)
		require.Len(t, *field.Addresses, 1)
		assert.Len(t, (*field.Addresses)[0].Name, nameBytes)
		assert.Equal(t, address, (*field.Addresses)[0].Address)
		assert.Equal(t, capacity, d.headerDisplayBytes)
	}
}

func TestInvalidGroupSeparationRemainsExplicitInCanonicalEvidence(t *testing.T) {
	for _, value := range []string{
		"Friends:a@example.test; b@example.test",
		"Friends:a@example.test; Other:b@example.test;",
		"",
		"(comment only)",
	} {
		t.Run(value, func(t *testing.T) {
			result := decodePublicFixture(t, []byte("From: "+value+"\r\n\r\nbody"))
			field := result.Evidence.Inventory.Messages[0].Fields.From[0]
			assert.Equal(t, document.EmailInterpretationInvalid, field.State)
			assert.Nil(t, field.Addresses)
		})
	}
}

func TestDiagnosticExhaustionEvictsInvalidAddressForEssentialMedia(t *testing.T) {
	raw := strings.Repeat("From: bad\r\n", 4094) + "Date: 1 Jan 2020 00:00:00 +0000\r\nContent-Type: multipart/mixed; boundary=outer\r\n\r\n" +
		"--outer\r\nContent-Type: message/rfc822\r\n\r\nFrom: bad\r\nFrom: bad\r\nDate: 1 Jan 2020 00:00:00 +0000\r\nContent-Type: multipart/mixed; boundary=inner\r\n\r\n" +
		"--inner\r\nContent-Type: invalid@type\r\n\r\nbody\r\n--inner--\r\n--outer--\r\n"
	result := decodePublicFixture(t, []byte(raw))
	assert.Equal(t, document.EmailInventoryPartial, result.Evidence.Inventory.State)
	require.NotNil(t, result.Evidence.Inventory.Termination)
	assert.Equal(t, document.EmailDiagnosticLimit, result.Evidence.Inventory.Termination.Code)
	parts := result.Evidence.Inventory.Parts
	require.NotEmpty(t, parts)
	require.Len(t, parts[len(parts)-1].Media.Diagnostics, 1)
	assert.Equal(t, document.EmailDiagnosticInvalidHeader, parts[len(parts)-1].Media.Diagnostics[0].Code)
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

func TestAddressDiscardedSyntaxDoesNotConsumeDisplayCapacity(t *testing.T) {
	const address = "a@example.test"
	for _, name := range []string{
		`"` + strings.Repeat(`\a`, 128<<10) + `"`,
		strings.Repeat("=?utf-8?B?YQ==?= ", 8192),
		"Group " + strings.Repeat("=?utf-8?Q??= ", 8192),
	} {
		for _, member := range []string{"", address} {
			value := name + ":" + member + "; (" + strings.Repeat(`\a`, 128<<10) + ")"
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
