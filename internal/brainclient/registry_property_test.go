//go:build !darwin || arm64

package brainclient

import (
	"fmt"
	"io/fs"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"hegel.dev/go/hegel"
)

// Hegel's native engine runs on supported Linux/Windows and macOS arm64.
// The deterministic registry/process tests run on every supported platform.
func TestBrainRegistryRoundTripProperty(t *testing.T) {
	root := t.TempDir()
	hegel.Test(t, func(ht *hegel.T) {
		dir, err := os.MkdirTemp(root, "case-")
		require.NoError(ht, err)
		defer func() { require.NoError(ht, os.RemoveAll(dir)) }()
		r, err := OpenRegistry(filepath.Join(dir, "registry"))
		require.NoError(ht, err)
		count := hegel.Draw(ht, hegel.Integers(0, 6))
		want := make([]ConnectionConfig, 0, count)
		for i := range count {
			suffix := hegel.Draw(ht, hegel.Text().Alphabet("abcdefghijklmnopqrstuvwxyz0123456789-_.").MinSize(1).MaxSize(20))
			leaf := hegel.Draw(ht, hegel.Text().Alphabet("abcdefghijklmnopqrstuvwxyz αβé中-_$%&'").MinSize(1).MaxSize(30))
			c := ConnectionConfig{Name: fmt.Sprintf("note-%d-%s", i, suffix), SourceRoot: filepath.Join(dir, "source", leaf), Destination: "/knowledge/α", Exclude: hegel.Draw(ht, hegel.Lists(hegel.SampledFrom([]string{".git", "cache/", "folder/child", "*.literal", "α"})).MaxSize(5)), SettleTime: time.Duration(hegel.Draw(ht, hegel.Integers(int64(0), int64(math.MaxInt64)))), MinimumAge: time.Duration(hegel.Draw(ht, hegel.Integers(int64(0), int64(math.MaxInt64)))), ScanInterval: time.Duration(hegel.Draw(ht, hegel.Integers(int64(1), int64(math.MaxInt64)))), IndexLocal: hegel.Draw(ht, hegel.Booleans())}
			if len(c.Exclude) == 0 {
				c.Exclude = []string{}
			}
			if hegel.Draw(ht, hegel.Booleans()) {
				c.LocalVaultRoot = filepath.Join(dir, "vault", leaf)
			} else {
				c.RemoteOrigin = "https://daemon.example:9443"
				if hegel.Draw(ht, hegel.Booleans()) {
					c.KeyFile = filepath.Join(dir, "keys", leaf)
				}
			}
			want = append(want, c)
		}
		// Reverse the authored insertion order; expected name order comes from
		// ordinary string ordering, not a registry serialization helper.
		for i := len(want) - 1; i >= 0; i-- {
			require.NoError(ht, r.Put(t.Context(), want[i]))
		}
		fresh, err := OpenRegistry(filepath.Join(dir, "registry"))
		require.NoError(ht, err)
		saved, err := fresh.List(t.Context())
		require.NoError(ht, err)
		sort.Slice(want, func(i, j int) bool { return want[i].Name < want[j].Name })
		require.Equal(ht, want, saved)
		for _, c := range want {
			got, err := fresh.Get(t.Context(), c.Name)
			require.NoError(ht, err)
			require.Equal(ht, c, got)
			require.NoError(ht, fresh.Put(t.Context(), c))
		}
	}, hegel.WithDatabase(t.TempDir()), hegel.WithTestCases(2000))
}

// Invalid names must be rejected before any write, with an independent grammar
// oracle rather than calling the production name validator.
func TestBrainRegistryNameValidationProperty(t *testing.T) {
	root := t.TempDir()
	validName := regexp.MustCompile(`^[a-z0-9._-]{1,64}$`)
	hegel.Test(t, func(ht *hegel.T) {
		dir, err := os.MkdirTemp(root, "case-")
		require.NoError(ht, err)
		defer func() { require.NoError(ht, os.RemoveAll(dir)) }()
		name := hegel.Draw(ht, hegel.Text().MaxSize(80))
		r, err := OpenRegistry(filepath.Join(dir, "registry"))
		require.NoError(ht, err)
		cfg := ConnectionConfig{Name: name, SourceRoot: filepath.Join(dir, "source"), Destination: "/knowledge", LocalVaultRoot: filepath.Join(dir, "vault"), ScanInterval: time.Nanosecond}
		err = r.Put(t.Context(), cfg)
		if validName.MatchString(name) {
			require.NoError(ht, err)
			got, e := r.Get(t.Context(), name)
			require.NoError(ht, e)
			require.Equal(ht, cfg.Name, got.Name)
		} else {
			require.Error(ht, err)
			_, e := os.Stat(filepath.Join(dir, "registry"))
			require.ErrorIs(ht, e, fs.ErrNotExist)
		}
	}, hegel.WithDatabase(t.TempDir()), hegel.WithTestCases(2000))
}
