package push_test

import (
	"context"
	"crypto/sha256"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/internal/daemonauth"
	"go.kenn.io/docbank/internal/daemonconn"
	"go.kenn.io/docbank/internal/push"
	"go.kenn.io/docbank/internal/store"
	"go.kenn.io/kit/packstore"
)

func TestPushOverlapsUploadsAndHandlesCancellation(t *testing.T) {
	for _, canceled := range []bool{false, true} {
		t.Run(fmt.Sprintf("canceled=%t", canceled), func(t *testing.T) {
			f := newFixture(t)
			source := t.TempDir()
			var names []string
			stripes := map[byte]bool{}
			// Distinct content stripes keep the fixture free to fill every upload slot.
			for i := 0; len(names) < 65; i++ {
				name := fmt.Sprintf("file-%04d.bin", i)
				digest := sha256.Sum256([]byte(name))
				if stripes[digest[0]] {
					continue
				}
				stripes[digest[0]] = true
				names = append(names, name)
				require.NoError(t, os.WriteFile(filepath.Join(source, name), []byte(name), 0o600))
			}
			arrived := make(chan struct{}, 65)
			release := make(chan struct{})
			var active, peak, proofs atomic.Int64
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == daemonauth.KeyChallengePath {
					proofs.Add(1)
				}
				if r.URL.Path == "/api/v1/push/uploads" {
					n := active.Add(1)
					defer active.Add(-1)
					for old := peak.Load(); n > old; old = peak.Load() {
						if peak.CompareAndSwap(old, n) {
							break
						}
					}
					arrived <- struct{}{}
					select {
					case <-release:
					case <-r.Context().Done():
						return
					}
					if canceled {
						return
					}
				}
				f.server.Config.Handler.ServeHTTP(w, r)
			}))
			defer server.Close()
			connection, err := daemonconn.NewPushConnection(server.URL, "synthetic-key")
			require.NoError(t, err)
			defer func() { require.NoError(t, connection.Close()) }()
			ctx, cancel := context.WithCancel(t.Context())
			var releaseOnce sync.Once
			unblock := func() { releaseOnce.Do(func() { close(release) }) }
			done := make(chan struct{})
			var report push.Report
			var runErr error
			opts := options(source)
			var acknowledged []string // The caller's callbacks must remain serialized.
			opts.Progress = func(ref, _ string) error {
				acknowledged = append(acknowledged, ref)
				return nil
			}
			go func() {
				defer close(done)
				report, runErr = push.Run(ctx, connection, opts)
			}()
			defer func() { cancel(); unblock(); <-done }()
			arrivalDeadline := time.NewTimer(10 * time.Second)
			defer arrivalDeadline.Stop()
			for i := range 32 {
				select {
				case <-arrived:
				case <-arrivalDeadline.C:
					t.Fatalf("only %d uploads reached the daemon concurrently", i)
				}
			}
			if canceled {
				cancel()
			} else {
				unblock()
			}
			completionDeadline := time.NewTimer(30 * time.Second)
			defer completionDeadline.Stop()
			select {
			case <-done:
			case <-completionDeadline.C:
				t.Fatal("push did not finish")
			}
			unblock()
			server.Close()
			require.EqualValues(t, 32, peak.Load())
			if canceled {
				require.ErrorIs(t, runErr, context.Canceled)
				require.Equal(t, push.Report{}, report)
				require.Empty(t, acknowledged)
				return
			}
			require.NoError(t, runErr)
			require.EqualValues(t, 32, proofs.Load(), "reuse authenticated connections across windows")
			require.Equal(t, push.Report{Added: 65}, report)
			require.Len(t, acknowledged, 65)
			for _, name := range names {
				node, err := f.store.NodeByPath(t.Context(), "/archive/"+name)
				require.NoError(t, err)
				require.Equal(t, name, readNode(t, f, node.ID))
			}
		})
	}
}

func TestPushRecoversIdenticalContentWithoutPhysicalAuthority(t *testing.T) {
	f := newFixture(t)
	source := t.TempDir()
	content := []byte(strings.Repeat("synthetic shared content", 512))
	require.NoError(t, os.WriteFile(filepath.Join(source, "seed.bin"), content, 0o600))
	_, err := push.Run(t.Context(), f.connection, options(source))
	require.NoError(t, err)
	seed, err := f.store.NodeByPath(t.Context(), "/archive/seed.bin")
	require.NoError(t, err)
	_, err = f.blobs.Maintainer().Pack(t.Context(), packstore.PackOptions{})
	require.NoError(t, err)
	require.NoError(t, store.NewPackCatalog(f.store).DeleteIndexEntry(t.Context(), packstore.Hash(seed.BlobHash)))
	_, err = f.store.PhysicalContent(t.Context(), seed.BlobHash)
	require.ErrorIs(t, err, store.ErrPhysicalAuthorityMissing)
	for i := range 8 {
		require.NoError(t, os.WriteFile(filepath.Join(source, fmt.Sprintf("copy-%d.bin", i)), content, 0o600))
	}
	opts := options(source)
	opts.Folder.Name, opts.Folder.Destination = "recovery", "/recovery"
	report, err := push.Run(t.Context(), f.connection, opts)
	require.NoError(t, err)
	require.Equal(t, push.Report{Added: 9}, report)
	for i := range 8 {
		node, err := f.store.NodeByPath(t.Context(), fmt.Sprintf("/recovery/copy-%d.bin", i))
		require.NoError(t, err)
		require.Equal(t, string(content), readNode(t, f, node.ID))
	}
}
