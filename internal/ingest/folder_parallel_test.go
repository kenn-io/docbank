package ingest

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/internal/config"
)

func TestFolderScanJoinsCanceledProcessorBeforeClosingSource(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{"a.txt", "b.txt"} {
		require.NoError(t, os.WriteFile(filepath.Join(dir, name), []byte("source"), 0o600))
	}
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		entered, release := make(chan struct{}), make(chan struct{})
		var source *os.File
		scanner, err := NewFolderScanner(config.WatchConfig{
			Name: "synthetic", Source: dir, Destination: "/archive",
			SettleTime: config.Duration(time.Second), ScanInterval: config.Duration(time.Second),
		}, func(ctx context.Context, file FolderFile) error {
			source = file.File
			close(entered)
			<-ctx.Done()
			<-release
			_, err := file.File.Stat()
			return errors.Join(ctx.Err(), err)
		}, failOnReject(t))
		require.NoError(t, err)
		scanner.watcher.beforeObserve = func(ref string) {
			if ref == "b.txt" {
				<-entered
				<-ctx.Done()
			}
		}
		done := make(chan error, 1)
		go func() { done <- scanner.ScanOnce(ctx, 8) }()
		<-entered
		cancel()
		synctest.Wait()
		returned := false
		select {
		case err = <-done:
			returned = true
			t.Error("scan returned while its processor still owned the source")
		default:
		}
		close(release)
		if !returned {
			err = <-done
		}
		synctest.Wait()
		require.ErrorIs(t, err, context.Canceled)
		require.NotErrorIs(t, err, os.ErrClosed)
		_, err = source.Stat()
		require.ErrorIs(t, err, os.ErrClosed)
	})
}

func TestFolderScanOrdersConflictingNormalizedPaths(t *testing.T) {
	for _, refs := range [][2]string{
		{"cafe\u0301.txt", "caf\u00e9.txt"},
		{"cafe\u0301/a.txt", "caf\u00e9"},
		{"cafe\u0301", "caf\u00e9/a.txt"},
	} {
		t.Run(strings.Join(refs[:], ","), func(t *testing.T) {
			dir := t.TempDir()
			for i, ref := range refs {
				if i == 1 {
					_, err := os.Stat(filepath.Join(dir, strings.Split(ref, "/")[0]))
					if err == nil {
						t.Skip("filesystem aliases the two Unicode spellings")
					}
					require.ErrorIs(t, err, os.ErrNotExist)
				}
				name := filepath.Join(dir, filepath.FromSlash(ref))
				require.NoError(t, os.MkdirAll(filepath.Dir(name), 0o700))
				require.NoError(t, os.WriteFile(name, []byte(ref), 0o600))
			}
			synctest.Test(t, func(t *testing.T) {
				entered := make(chan string, 2)
				release := make(chan struct{})
				secondObserved := make(chan struct{})
				scanner, err := NewFolderScanner(config.WatchConfig{
					Name: "synthetic", Source: dir, Destination: "/archive",
					SettleTime: config.Duration(time.Second), ScanInterval: config.Duration(time.Second),
				}, func(_ context.Context, file FolderFile) error {
					entered <- file.Ref
					if file.Ref == refs[0] {
						<-release
					}
					data, err := io.ReadAll(file.File)
					if err == nil && string(data) != file.Ref {
						return fmt.Errorf("source %q has unexpected bytes", file.Ref)
					}
					return err
				}, failOnReject(t))
				require.NoError(t, err)
				scanner.watcher.beforeObserve = func(ref string) {
					if ref == refs[1] {
						close(secondObserved)
					}
				}
				done := make(chan error, 1)
				go func() { done <- scanner.ScanOnce(t.Context(), 8) }()
				<-secondObserved
				synctest.Wait()
				first := <-entered
				var early string
				select {
				case ref := <-entered:
					early = ref
				default:
				}
				close(release)
				require.NoError(t, <-done)
				require.Equal(t, refs[0], first)
				require.Empty(t, early, "conflicting source started before its predecessor finished")
				require.Equal(t, refs[1], <-entered)
			})
		})
	}
}
