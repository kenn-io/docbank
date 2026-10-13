package store

import (
	"database/sql"
	"fmt"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

// BenchmarkMailboxJobs measures complete history pages, excluding fixture writes.
func BenchmarkMailboxJobs(b *testing.B) {
	for _, jobs := range []int{100, 1000} {
		for _, entries := range []int{0, 1000} {
			b.Run(fmt.Sprintf("jobs=%d/entries=%d", jobs, entries), func(b *testing.B) {
				s, err := Open(filepath.Join(b.TempDir(), "docbank.db"))
				require.NoError(b, err)
				b.Cleanup(func() { require.NoError(b, s.Close()) })
				ctx := b.Context()
				hash := fmt.Sprintf("%064x", 1)
				_, err = s.CreateFile(ctx, s.RootID(), "source.mbox", hash, 128, "application/mbox")
				require.NoError(b, err)
				container, err := s.BeginMailboxContainer(ctx, MailboxContainerRequest{
					ID: "source", Owner: "synthetic-owner", SHA256: hash, Size: 128, Format: "mbox",
				})
				require.NoError(b, err)
				require.NoError(b, s.PutMailboxChunk(ctx, container.Owner, container.ID,
					MailboxChunk{SHA256: hash, Size: 128}))
				_, err = s.SealMailboxContainer(ctx, container.Owner, container.ID, hash, 128)
				require.NoError(b, err)
				hashes := make([]string, entries)
				for i := range hashes {
					hashes[i] = fmt.Sprintf("%064x", i+1)
				}
				require.NoError(b, s.withStorageTx(ctx, func(tx *sql.Tx) error {
					for i := range jobs {
						job := MailboxJob{
							ID: fmt.Sprintf("job-%06d", i), ContainerID: container.ID, ContainerSHA256: hash,
							Settings: MailboxSettings{Recipe: mailboxRecipe(), Dialect: "mboxrd", DestinationID: s.RootID()},
							Owner:    container.Owner, State: "complete", ScannedTail: true, EntryHashes: hashes,
							CollectionID: fmt.Sprintf("%08x-1111-4111-8111-111111111111", i+1),
							StartedAt:    "2026-01-01T00:00:00.000000000Z",
						}
						if err := saveMailboxJob(ctx, tx, job, true); err != nil {
							return err
						}
					}
					return nil
				}))
				b.Run("single_job", func(b *testing.B) {
					b.ReportAllocs()
					for b.Loop() {
						job, err := s.MailboxJob(ctx, container.Owner, "job-000000")
						if err != nil || job.ID != "job-000000" || len(job.EntryHashes) != entries {
							b.Fatalf("unexpected job: id=%q err=%v", job.ID, err)
						}
					}
				})
				for _, limit := range []int{1, 20, 100} {
					b.Run(fmt.Sprintf("limit=%d", limit), func(b *testing.B) {
						firstID, lastID := fmt.Sprintf("job-%06d", jobs-1), fmt.Sprintf("job-%06d", jobs-limit)
						b.ReportAllocs()
						for b.Loop() {
							page, err := s.MailboxJobs(ctx, container.Owner, "", limit)
							if err != nil || len(page) != limit {
								b.Fatalf("jobs=%d err=%v", len(page), err)
							}
							if page[0].ID != firstID || page[len(page)-1].ID != lastID ||
								len(page[0].EntryHashes) != entries || page[0].State != "complete" {
								b.Fatal("unexpected history page")
							}
						}
					})
				}
			})
		}
	}
}
