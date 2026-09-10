package emailmime

import (
	"encoding/base64"
	"net/mail"
	"runtime"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/docbank/document"
)

func TestAddressProjectionRefusesLargeMailboxBeforeAllocatingIt(t *testing.T) {
	const nameBytes = 512 << 10
	mailboxName := strings.Repeat("a", nameBytes)
	for _, budget := range []int64{0, 7} {
		for name, value := range map[string]string{
			"encoded word": "=?utf-8?B?" + base64.StdEncoding.EncodeToString([]byte(mailboxName)) + "?= <a@example.test>",
			"quoted":       `"` + mailboxName + `" <a@example.test>`,
			"quoted pairs": `"` + strings.Repeat(`\a`, nameBytes) + `" <a@example.test>`,
		} {
			t.Run(name+"/budget="+string(rune('0'+budget)), func(t *testing.T) {
				runtime.GC()
				var before, after runtime.MemStats
				runtime.ReadMemStats(&before)
				_, _, err := projectAddressesBounded(value, budget)
				runtime.ReadMemStats(&after)
				require.ErrorIs(t, err, errDisplayBudget)
				assert.Less(t, after.TotalAlloc-before.TotalAlloc, uint64(128<<10), "refusal allocated a mailbox-sized intermediate")
			})
		}
	}
}

func TestAddressProjectionPreservesActualLargeMailboxCapacity(t *testing.T) {
	const nameBytes = 512 << 10
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

func TestAddressProjectionPreservesMailGrammar(t *testing.T) {
	valid := []string{
		"A <a@example.test>, b@example.test",
		`"Last, First" <person@example.test>`,
		`John (middle) Doe <jdoe@machine.example>`,
		`"Giant; \"Big\" Box" <sysservices@example.net>`,
		`=?utf-8?q?J=C3=B6rg_Doe?= <joerg@example.test>`,
		`=?utf-8?q?J=C3=B6rg?=  =?utf-8?q?Doe?= <joerg@example.test>`,
		"Friends: A <a@example.test>, b@example.test;",
		"Friends: A <a@example.test>; (trailing comment)",
		"undisclosed-recipients:;",
		"Group1: <addr1@example.test>;, Group 2: addr2@example.test;, John <addr3@example.test>",
		" , joe@example.test,,John <jdoe@example.test>,,",
		"a@example.test (display comment)",
		"a@example.test (nested (display) comment)",
		"user@[IPv6:2001:db8::1]",
	}
	invalid := []string{
		"Friends:a@example.test; b@example.test",
		"Friends:a@example.test; Other:b@example.test;",
		"",
		"(comment only)",
		"John Doe",
		"a@example.test b@example.test",
		"group not closed: a@example.test",
		"root group: embed group: a@example.test;",
		"a@example.test (",
		`"\` + "\x00" + `" <null@example.test>`,
		`=?unknown?q?name?= <a@example.test>`,
		`=?windows-1252?q?name?= <a@example.test>`,
		`a@example.test (=?unknown?q?name?=)`,
	}
	for _, value := range append(valid, invalid...) {
		t.Run(value, func(t *testing.T) {
			want, wantErr := mail.ParseAddressList(value)
			got, _, gotErr := projectAddressesBounded(value, 1<<20)
			require.Equal(t, wantErr == nil, gotErr == nil, "reference error: %v; bounded error: %v", wantErr, gotErr)
			if wantErr == nil {
				require.Len(t, got, len(want))
				for index := range want {
					assert.Equal(t, want[index].Name, got[index].Name)
					assert.Equal(t, want[index].Address, got[index].Address)
				}
			}
		})
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

func TestQuotedPairEncodedFilenameUsesDecodedCapacity(t *testing.T) {
	for _, delta := range []int64{-1, 0, 1} {
		d := &decoder{ctx: t.Context(), limits: Recipe().Limits}
		d.limits.HeaderDisplayBytes = int64(len("abc")) + delta
		filename, diagnostics := d.interpretFilename("1", parsedHeaderBlock{fields: []parsedHeader{{index: 0, name: "content-disposition", value: `attachment; filename="\=?utf-8?B?YWJj?="`, valid: true}}})
		if delta < 0 {
			assert.Equal(t, document.EmailInterpretationUnsupported, filename.State)
			require.Len(t, diagnostics, 1)
			assert.Equal(t, document.EmailDiagnosticHeaderDisplayLimit, diagnostics[0].Code)
			continue
		}
		assert.Equal(t, document.EmailInterpretationDecoded, filename.State)
		require.NotNil(t, filename.Decoded)
		assert.Equal(t, "abc", *filename.Decoded)
		assert.Equal(t, int64(3), d.headerDisplayBytes)
	}
}

func TestQuotedPairEncodedFilenameAtPublicAggregateBoundary(t *testing.T) {
	const limit = 1 << 20
	raw := "Subject: " + strings.Repeat("a", limit/2) + "\r\nContent-Type: multipart/mixed; boundary=outer\r\n\r\n" +
		"--outer\r\nContent-Type: message/rfc822\r\n\r\nSubject: " + strings.Repeat("b", limit/2-3) + "\r\nContent-Type: multipart/mixed; boundary=inner\r\n\r\n" +
		"--inner\r\nContent-Type: application/octet-stream\r\nContent-Disposition: attachment; filename=\"\\=?utf-8?B?YWJj?=\"\r\n\r\nbody\r\n--inner--\r\n--outer--\r\n"
	result := decodePublicFixture(t, []byte(raw))
	filename := result.Evidence.Inventory.Parts[len(result.Evidence.Inventory.Parts)-1].Filename
	assert.Equal(t, document.EmailInterpretationDecoded, filename.State)
	require.NotNil(t, filename.Decoded)
	assert.Equal(t, "abc", *filename.Decoded)
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
