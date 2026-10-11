package store

import (
	"database/sql"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// Hold the duplicate groups fixed while unrelated content grows. Measurements
// include population totals, paths, and collection previews for the page.
func BenchmarkDuplicates(b *testing.B) {
	for _, unrelated := range []int{0, 10000} {
		b.Run(fmt.Sprintf("unrelated=%d", unrelated), func(b *testing.B) {
			s, err := Open(filepath.Join(b.TempDir(), "docbank.db"))
			require.NoError(b, err)
			b.Cleanup(func() { require.NoError(b, s.Close()) })
			ctx := b.Context()
			hash := strings.Repeat("a1", 32)
			var first, second Node
			require.NoError(b, s.withStorageTx(ctx, func(tx *sql.Tx) error {
				for i := range 2 + unrelated {
					contentHash := hash
					if i >= 2 {
						contentHash = fmt.Sprintf("%064x", i)
					}
					node, _, err := s.createFileTx(ctx, tx, s.RootID(),
						fmt.Sprintf("file-%05d.bin", i), contentHash, 1024, "application/octet-stream")
					if err != nil {
						return err
					}
					switch i {
					case 0:
						first = node
					case 1:
						second = node
					}
				}
				return nil
			}))
			b.ReportAllocs()
			for b.Loop() {
				page, err := s.Duplicates(ctx, 50, 0)
				if err != nil || page.Total != 1 || page.TotalReferences != 2 || len(page.Items) != 1 {
					b.Fatalf("page=%+v err=%v", page, err)
				}
				group := page.Items[0]
				if group.ReferenceCount != 2 || len(group.References) != 2 ||
					group.RepresentativeNodeID != first.ID || group.ReferencesTruncated {
					b.Fatalf("group=%+v err=%v", group, err)
				}
				if group.References[0].Reference.Node.ID != first.ID ||
					group.References[1].Reference.Node.ID != second.ID ||
					group.References[0].Reference.Path != "/file-00000.bin" ||
					group.References[1].Reference.Path != "/file-00001.bin" {
					b.Fatal("duplicate group identities or paths changed")
				}
			}
		})
	}
}
