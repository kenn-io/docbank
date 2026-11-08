package brainservice

import (
	"context"
	"encoding/binary"
	"io/fs"
	"strings"
	"testing"
	"unicode/utf16"

	"github.com/stretchr/testify/require"
)

func TestBrainServiceFailedNotFoundIsUnknown(t *testing.T) {
	spec := renderSpec("linux")
	d := newServiceDriver("linux", func(context.Context, string, ...string) ([]byte, error) {
		return []byte("LoadState=not-found\nActiveState=inactive\nFragmentPath=\n"), fs.ErrPermission
	}, nil)
	status, err := d.Status(t.Context(), spec)
	require.ErrorIs(t, err, ErrManager)
	require.Equal(t, "unknown", status.State)
}
func TestBrainServiceTaskXMLPreservesActionWhitespace(t *testing.T) {
	reg, err := Render("windows", renderSpec("windows"))
	require.NoError(t, err)
	altered := strings.Replace(string(reg.Definition), "<Command>", "<Command> ", 1)
	equal, err := equalTaskXML([]byte(altered), reg.Definition)
	require.NoError(t, err)
	require.False(t, equal, "ownership must not trim executable/action values")
}
func TestBrainServiceTaskXML(t *testing.T) {
	reg, err := Render("windows", renderSpec("windows"))
	require.NoError(t, err)
	formatted := strings.ReplaceAll(string(reg.Definition), "><", ">\n  <")
	equal, err := equalTaskXML([]byte(formatted), reg.Definition)
	require.NoError(t, err)
	require.True(t, equal)
	// Native schtasks may emit UTF-16 with a BOM. Decode independently here.
	words := utf16.Encode([]rune(strings.Replace(string(reg.Definition), `encoding="UTF-8"`, `encoding="UTF-16"`, 1)))
	data := []byte{0xff, 0xfe}
	for _, word := range words {
		data = binary.LittleEndian.AppendUint16(data, word)
	}
	equal, err = equalTaskXML(data, reg.Definition)
	require.NoError(t, err)
	require.True(t, equal)
	for _, bad := range [][]byte{[]byte("not xml"), []byte(`<Task/>`), append(data, 0), []byte(strings.Repeat("x", maxDefinition+1))} {
		_, err := equalTaskXML(bad, reg.Definition)
		require.Error(t, err)
	}
	for _, changed := range []string{
		strings.Replace(string(reg.Definition), "LeastPrivilege", "HighestAvailable", 1),
		strings.Replace(string(reg.Definition), "<Settings>", "<Settings><RestartOnFailure/>", 1),
		strings.Replace(string(reg.Definition), "--config-root", "--other-root", 1),
	} {
		equal, err := equalTaskXML([]byte(changed), reg.Definition)
		require.NoError(t, err)
		require.False(t, equal)
	}
}
func TestBrainServiceQueryBoundsAndCancellation(t *testing.T) {
	for _, goos := range []string{"linux", "darwin", "windows"} {
		t.Run(goos, func(t *testing.T) {
			calls := 0
			d := newServiceDriver(goos, func(context.Context, string, ...string) ([]byte, error) {
				calls++
				return []byte(strings.Repeat("x", maxDefinition+1)), nil
			}, func(context.Context, string) (int, error) { return 4, nil })
			status, err := d.Status(t.Context(), renderSpec(goos))
			require.ErrorIs(t, err, ErrOutputLimit)
			require.Equal(t, "unknown", status.State)
			ctx, cancel := context.WithCancel(t.Context())
			cancel()
			before := calls
			status, err = d.Status(ctx, renderSpec(goos))
			require.ErrorIs(t, err, context.Canceled)
			require.Equal(t, "unknown", status.State)
			require.Equal(t, before, calls)
		})
	}
}
func TestBrainServiceWindowsStateErrors(t *testing.T) {
	for _, state := range []int{0, 1, 2, 3, 4, 5} {
		spec := renderSpec("windows")
		reg, err := Render("windows", spec)
		require.NoError(t, err)
		d := newServiceDriver("windows", func(context.Context, string, ...string) ([]byte, error) { return reg.Definition, nil }, func(context.Context, string) (int, error) { return state, nil })
		installed, running, matched, err := d.query(t.Context(), spec, reg)
		if state < 1 || state > 4 {
			require.ErrorIs(t, err, ErrManager)
			continue
		}
		require.NoError(t, err)
		require.True(t, installed && matched)
		require.Equal(t, state == 4, running)
	}
	d := newServiceDriver("windows", func(context.Context, string, ...string) ([]byte, error) {
		t.Fatal("no query after COM failure")
		return nil, nil
	}, func(context.Context, string) (int, error) { return 0, fs.ErrPermission })
	_, err := d.Status(t.Context(), renderSpec("windows"))
	require.ErrorIs(t, err, fs.ErrPermission)
}
