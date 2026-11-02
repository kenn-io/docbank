package fsname

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCheck(t *testing.T) {
	tests := []struct {
		name    string
		in      string
		wantErr bool
	}{
		{name: "plain", in: "report.txt"},
		{name: "unicode", in: "résumé.pdf"},
		{name: "leading dot", in: ".config"},
		{name: "dot dot prefix is a name", in: "..foo"},
		{name: "empty", in: "", wantErr: true},
		{name: "dot", in: ".", wantErr: true},
		{name: "dot dot", in: "..", wantErr: true},
		{name: "slash", in: "a/b", wantErr: true},
		{name: "backslash", in: `a\b`, wantErr: true},
		{name: "alternate data stream", in: "a:b", wantErr: true},
		{name: "wildcard", in: "a*b", wantErr: true},
		{name: "control character", in: "a\x01b", wantErr: true},
		{name: "invalid utf8", in: "a\xffb", wantErr: true},
		{name: "trailing dot", in: "notes.", wantErr: true},
		{name: "trailing space", in: "notes ", wantErr: true},
		{name: "leading space", in: " notes", wantErr: true},
		{name: "device", in: "NUL", wantErr: true},
		{name: "device lower case", in: "nul", wantErr: true},
		{name: "device with extension", in: "CON.txt", wantErr: true},
		{name: "superscript port", in: "COM¹", wantErr: true},
		{name: "console input", in: "CONIN$", wantErr: true},
		{name: "console output mixed case", in: "ConOut$", wantErr: true},
		{name: "console with extension", in: "conin$.log", wantErr: true},
		{name: "console with space before extension", in: "CONOUT$ .log", wantErr: true},
		{name: "console prefix is a name", in: "CONIN$X"},
		{name: "too long", in: strings.Repeat("a", 256), wantErr: true},
		{name: "max length", in: strings.Repeat("a", 255)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := Check(tt.in)

			if tt.wantErr {
				assert.ErrorIs(t, err, ErrNotPortable)
				return
			}
			assert.NoError(t, err)
		})
	}
}

func TestCleanProducesCheckedNames(t *testing.T) {
	tests := []struct {
		in   string
		want string
	}{
		{in: "report:final.txt", want: "reportfinal.txt"},
		{in: "CON.txt", want: "CON_.txt"},
		{in: "CONIN$", want: "CONIN$_"},
		{in: "conout$.log", want: "conout$_.log"},
		{in: "CONOUT$ .log", want: "CONOUT$_.log"},
		{in: "", want: "file"},
		{in: "  notes.  ", want: "notes"},
	}
	for _, tt := range tests {
		t.Run(tt.in, func(t *testing.T) {
			got := Clean(tt.in)

			require.NoError(t, Check(got))
			assert.Equal(t, tt.want, got)
			assert.Equal(t, got, Clean(got), "Clean must be idempotent")
		})
	}
}

func TestCleanKeepsDefusedConsoleNameWithinLengthLimit(t *testing.T) {
	name := "CONIN$." + strings.Repeat("a", 255-len("CONIN$."))

	got := Clean(name)

	assert.LessOrEqual(t, len(got), 255)
	assert.NoError(t, Check(got))
}

func TestJoin(t *testing.T) {
	tests := []struct {
		name  string
		parts []string
		want  string
	}{
		{name: "plain", parts: []string{"a", "b.txt"}, want: filepath.Join("a", "b.txt")},
		{name: "embedded separators", parts: []string{`a/b\c`}, want: filepath.Join("a", "b", "c")},
		{name: "traversal dropped", parts: []string{"../../etc", "passwd"}, want: filepath.Join("etc", "passwd")},
		{name: "absolute made relative", parts: []string{"/abs", "x"}, want: filepath.Join("abs", "x")},
		{name: "drive letter cleaned", parts: []string{`C:\Windows`}, want: filepath.Join("C", "Windows")},
		{name: "device defused", parts: []string{"CONIN$", "NUL.txt"}, want: filepath.Join("CONIN$_", "NUL_.txt")},
		{name: "nothing usable", parts: []string{"..", "/", "."}, want: "file"},
		{name: "no parts", want: "file"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			require := require.New(t)

			got := Join(tt.parts...)

			require.Equal(tt.want, got)
			require.True(filepath.IsLocal(got), "Join result %q must be local", got)
		})
	}
}
