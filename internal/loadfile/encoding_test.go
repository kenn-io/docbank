package loadfile

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestEuroByteResolvesPerDeclaredEncoding(t *testing.T) {
	raw := []byte{0x80, 0x41}
	// 0x80 is the euro sign in Windows-1252 and U+0080 in ISO-8859-1, which
	// maps every byte to the code point of the same value. Both readings are
	// correct for their profile; the declared profile is what decides. The
	// ISO-8859-1 expectation is written as an escape, never as a literal
	// U+0080, which is invisible in review.
	for encoding, want := range map[string]string{"windows-1252": "€A", "iso-8859-1": "\u0080A"} {
		decode, err := Decoder(encoding)
		require.NoError(t, err)
		got, err := io.ReadAll(decode(bytes.NewReader(raw)))
		require.NoError(t, err)
		assert.Equal(t, want, string(got), encoding)
	}
	_, err := Decoder("shift-jis")
	require.ErrorIs(t, err, ErrInvalidProfile)
}

func TestDecoderRejectsInvalidBytesButKeepsLiteralReplacementRune(t *testing.T) {
	decode, err := Decoder("utf-8")
	require.NoError(t, err)
	got, err := io.ReadAll(decode(&oneByteReader{data: []byte("valid �")}))
	require.NoError(t, err)
	assert.Equal(t, "valid �", string(got))

	_, err = io.ReadAll(decode(&oneByteReader{data: []byte{'a', 0xff, 'b'}}))
	require.ErrorIs(t, err, ErrMalformedInput)
}

func TestDecoderValidatesSplitUTF16CodeUnits(t *testing.T) {
	for _, tc := range []struct {
		name     string
		encoding string
		raw      []byte
	}{
		{name: "little endian", encoding: "utf-16le", raw: []byte{0x41, 0x00, 0x3d, 0xd8, 0x00, 0xde}},
		{name: "big endian", encoding: "utf-16be", raw: []byte{0x00, 0x41, 0xd8, 0x3d, 0xde, 0x00}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			decode, err := Decoder(tc.encoding)
			require.NoError(t, err)
			got, err := io.ReadAll(decode(&oneByteReader{data: tc.raw}))
			require.NoError(t, err)
			assert.Equal(t, "A😀", string(got))
		})
	}

	decode, err := Decoder("utf-16le")
	require.NoError(t, err)
	for _, raw := range [][]byte{
		{0x41},
		{0x00, 0xdc},
		{0x00, 0xd8},
		{0x00, 0xd8, 0x41, 0x00},
	} {
		_, readErr := io.ReadAll(decode(&oneByteReader{data: raw}))
		require.ErrorIs(t, readErr, ErrMalformedInput, "%x", raw)
	}
}

func TestUTF16DecoderFactoryCanBeReusedConcurrently(t *testing.T) {
	for _, test := range []struct {
		name, encoding, want string
		unit                 []byte
	}{
		{name: "little endian", encoding: "utf-16le", unit: []byte{0x41, 0x00, 0x3d, 0xd8, 0x00, 0xde}, want: "A😀"},
		{name: "big endian", encoding: "utf-16be", unit: []byte{0x00, 0x41, 0xd8, 0x3d, 0xde, 0x00}, want: "A😀"},
	} {
		t.Run(test.name, func(t *testing.T) {
			decode, err := Decoder(test.encoding)
			require.NoError(t, err)
			raw := bytes.Repeat(test.unit, 1024)
			want := strings.Repeat(test.want, 1024)

			const workers = 32
			start := make(chan struct{})
			errors := make(chan error, workers)
			var group sync.WaitGroup
			group.Add(workers)
			for worker := range workers {
				go func() {
					defer group.Done()
					<-start
					for iteration := range 8 {
						decoded, err := io.ReadAll(decode(bytes.NewReader(raw)))
						if err != nil {
							errors <- fmt.Errorf("worker %d iteration %d: %w", worker, iteration, err)
							return
						}
						if string(decoded) != want {
							errors <- fmt.Errorf("worker %d iteration %d decoded inconsistent UTF-16", worker, iteration)
							return
						}
					}
				}()
			}
			close(start)
			group.Wait()
			close(errors)
			for err := range errors {
				require.NoError(t, err)
			}
		})
	}
}

func TestDecoderEnforcesDeclaredBOM(t *testing.T) {
	for _, tc := range []struct {
		name     string
		encoding string
		raw      []byte
		want     string
		wantErr  bool
	}{
		{name: "required UTF-8 BOM", encoding: "utf-8-bom", raw: append([]byte{0xef, 0xbb, 0xbf}, []byte("A")...), want: "A"},
		{name: "missing required UTF-8 BOM", encoding: "utf-8-bom", raw: []byte("A"), wantErr: true},
		{name: "plain UTF-8 rejects BOM", encoding: "utf-8", raw: append([]byte{0xef, 0xbb, 0xbf}, []byte("A")...), wantErr: true},
		{name: "UTF-16LE strips matching BOM", encoding: "utf-16le", raw: []byte{0xff, 0xfe, 0x41, 0x00}, want: "A"},
		{name: "UTF-16BE strips matching BOM", encoding: "utf-16be", raw: []byte{0xfe, 0xff, 0x00, 0x41}, want: "A"},
		{name: "UTF-16LE rejects opposite BOM", encoding: "utf-16le", raw: []byte{0xfe, 0xff, 0x00, 0x41}, wantErr: true},
		{name: "UTF-16BE rejects opposite BOM", encoding: "utf-16be", raw: []byte{0xff, 0xfe, 0x41, 0x00}, wantErr: true},
		{name: "legacy encoding rejects Unicode BOM", encoding: "windows-1252", raw: []byte{0xef, 0xbb, 0xbf, 0x41}, wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			decode, err := Decoder(tc.encoding)
			require.NoError(t, err)
			got, readErr := io.ReadAll(decode(&oneByteReader{data: tc.raw}))
			if tc.wantErr {
				require.ErrorIs(t, readErr, ErrMalformedInput)
				return
			}
			require.NoError(t, readErr)
			assert.Equal(t, tc.want, string(got))
		})
	}
}

func TestDecoderRejectsUTF32BOMs(t *testing.T) {
	encodings := []string{"utf-8", "utf-8-bom", "utf-16le", "utf-16be", "windows-1252", "iso-8859-1"}
	for byteOrder, raw := range map[string][]byte{
		"little endian": {0xff, 0xfe, 0x00, 0x00, 0x41, 0x00, 0x00, 0x00},
		"big endian":    {0x00, 0x00, 0xfe, 0xff, 0x00, 0x00, 0x00, 0x41},
	} {
		t.Run(byteOrder, func(t *testing.T) {
			for _, encoding := range encodings {
				decode, err := Decoder(encoding)
				require.NoError(t, err)
				_, err = io.ReadAll(decode(&oneByteReader{data: append([]byte(nil), raw...)}))
				require.ErrorIs(t, err, ErrMalformedInput, encoding)
			}
		})
	}
}

func TestDecoderRejectsUndefinedWindows1252Byte(t *testing.T) {
	decode, err := Decoder("windows-1252")
	require.NoError(t, err)
	_, err = io.ReadAll(decode(bytes.NewReader([]byte{0x81})))
	require.ErrorIs(t, err, ErrMalformedInput)

	decode, err = Decoder("utf-8")
	require.NoError(t, err)
	got, err := io.ReadAll(decode(bytes.NewReader([]byte{0xef, 0xbf, 0xbd})))
	require.NoError(t, err)
	assert.Equal(t, "�", string(got))
}

func TestDecoderPreservesUnderlyingReaderErrorAndBoundsReads(t *testing.T) {
	sourceErr := errors.New("source failed")
	source := &boundedErrorReader{data: []byte("complete"), err: sourceErr}
	decode, err := Decoder("utf-8")
	require.NoError(t, err)

	got, err := io.ReadAll(decode(source))
	assert.Equal(t, "complete", string(got))
	require.ErrorIs(t, err, sourceErr)
	assert.LessOrEqual(t, source.maxRequest, 4096)
}

type oneByteReader struct {
	data []byte
}

func (r *oneByteReader) Read(p []byte) (int, error) {
	if len(r.data) == 0 {
		return 0, io.EOF
	}
	p[0] = r.data[0]
	r.data = r.data[1:]
	return 1, nil
}

type boundedErrorReader struct {
	data       []byte
	err        error
	maxRequest int
}

func (r *boundedErrorReader) Read(p []byte) (int, error) {
	if len(p) > r.maxRequest {
		r.maxRequest = len(p)
	}
	if len(r.data) == 0 {
		return 0, r.err
	}
	n := copy(p, r.data)
	r.data = r.data[n:]
	if len(r.data) == 0 {
		return n, r.err
	}
	return n, nil
}
