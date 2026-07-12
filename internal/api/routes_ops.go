package api

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"path/filepath"
	"sort"
	"strings"

	"github.com/danielgtaylor/huma/v2"
	"go.kenn.io/kit/packstore"

	"go.kenn.io/docbank/internal/ingest"
)

func registerOpsRoutes(api huma.API, d Deps, g *gate) {
	type storageStatusOutput struct{ Body StorageStatus }
	huma.Register(api, huma.Operation{
		OperationID: "storageStatus", Method: http.MethodGet, Path: "/api/v1/storage",
		Summary: "Report loose and packed physical storage usage",
	}, func(ctx context.Context, _ *struct{}) (*storageStatusOutput, error) {
		stats, err := d.Blobs.Stats(ctx)
		if err != nil {
			return nil, FromStoreError(err)
		}
		return &storageStatusOutput{Body: StorageStatus{
			LooseBlobs: stats.LooseBlobs, LooseBytes: stats.LooseBytes,
			Packs: stats.Packs, PackStoredBytes: stats.PackStoredBytes,
			PackedBlobs: stats.PackedBlobs, PackedRawBytes: stats.PackedRawBytes,
			PackedStoredBytes: stats.PackedStoredBytes, DeadPackedBytes: stats.DeadPackedBytes,
		}}, nil
	})

	type storageUnpackOutput struct{ Body StorageUnpackReport }
	huma.Register(api, huma.Operation{
		OperationID: "storageUnpack", Method: http.MethodPost, Path: "/api/v1/storage/unpack",
		Summary: "Materialize live packed blobs loose and retire all pack files",
	}, func(ctx context.Context, _ *struct{}) (*storageUnpackOutput, error) {
		out := &storageUnpackOutput{}
		err := g.maintain(func() error {
			stats, err := d.Blobs.Maintainer().Unpack(ctx)
			if err != nil {
				return FromStoreError(err)
			}
			out.Body = storageUnpackReport(stats)
			return nil
		})
		return out, err
	})

	type storageRepackOutput struct{ Body StorageRepackReport }
	huma.Register(api, huma.Operation{
		OperationID: "storageRepack", Method: http.MethodPost, Path: "/api/v1/storage/repack",
		Summary: "Rewrite eligible sparse packs and retire dead pack files",
	}, func(ctx context.Context, in *struct {
		Body struct {
			MaxBytes     int64  `json:"max_bytes,omitempty" minimum:"0"`
			MinAge       string `json:"min_age,omitempty" example:"24h"`
			MinDeadBytes int64  `json:"min_dead_bytes,omitempty" minimum:"0"`
		}
	}) (*storageRepackOutput, error) {
		minAge, err := ParseAge(in.Body.MinAge)
		if err != nil {
			return nil, NewError(http.StatusUnprocessableEntity, "validation", err.Error())
		}
		out := &storageRepackOutput{}
		err = g.maintain(func() error {
			stats, err := d.Blobs.Maintainer().Repack(ctx, packstore.RepackOptions{
				MaxBytes: in.Body.MaxBytes,
				Selection: packstore.RepackSelection{
					MinAge: minAge, MinDeadStored: in.Body.MinDeadBytes,
				},
			})
			if err != nil {
				return FromStoreError(err)
			}
			out.Body = storageRepackReport(stats)
			return nil
		})
		return out, err
	})

	type storagePackOutput struct{ Body StoragePackReport }
	huma.Register(api, huma.Operation{
		OperationID: "storagePack", Method: http.MethodPost, Path: "/api/v1/storage/pack",
		Summary: "Pack authorized loose blobs into immutable pack files",
	}, func(ctx context.Context, in *struct {
		Body struct {
			MaxBytes int64 `json:"max_bytes,omitempty" minimum:"0"`
		}
	}) (*storagePackOutput, error) {
		out := &storagePackOutput{}
		err := g.maintain(func() error {
			stats, err := d.Blobs.Maintainer().Pack(ctx, packstore.PackOptions{MaxBytes: in.Body.MaxBytes})
			if err != nil {
				return FromStoreError(err)
			}
			out.Body = storagePackReport(stats)
			return nil
		})
		return out, err
	})

	type ingestOutput struct{ Body IngestReport }
	huma.Register(api, huma.Operation{
		OperationID: "ingest", Method: http.MethodPost, Path: "/api/v1/ingest",
		Summary: "Import server-side files or directory trees (loopback callers only)",
	}, func(ctx context.Context, in *struct {
		Body struct {
			Paths []string `json:"paths" minItems:"1"`
			Dest  string   `json:"dest" default:"/inbox"`
		}
	}) (*ingestOutput, error) {
		for _, p := range in.Body.Paths {
			if !filepath.IsAbs(p) {
				return nil, NewError(http.StatusUnprocessableEntity, "validation",
					fmt.Sprintf("path %q must be absolute: the daemon has no meaningful working directory", p))
			}
		}
		// The schema default covers an absent dest, not an explicit ""
		// (which MkdirAll would treat as the vault root).
		dest := in.Body.Dest
		if dest == "" {
			dest = "/inbox"
		}
		if !strings.HasPrefix(dest, "/") {
			return nil, NewError(http.StatusUnprocessableEntity, "validation",
				fmt.Sprintf("dest %q must be an absolute virtual path (start with /)", dest))
		}
		out := &ingestOutput{}
		err := g.mutate(func() error {
			return d.Blobs.WithMutation(ctx, func() error {
				ing := &ingest.Ingester{Store: d.Store, Blobs: d.Blobs}
				rep, err := ing.AddPaths(ctx, in.Body.Paths, dest)
				if err != nil {
					return FromStoreError(err)
				}
				out.Body = IngestReport{Added: rep.Added, Skipped: rep.Skipped}
				for _, f := range rep.Failed {
					out.Body.Failed = append(out.Body.Failed, IngestFailure{Path: f.Path, Error: f.Err.Error()})
				}
				return nil
			})
		})
		return out, err
	})

	type trashListOutput struct {
		Body struct {
			Items []Node `json:"items"`
		}
	}
	huma.Register(api, huma.Operation{
		OperationID: "listTrash", Method: http.MethodGet, Path: "/api/v1/trash",
		Summary: "List restorable trash roots, newest first",
	}, func(ctx context.Context, _ *struct{}) (*trashListOutput, error) {
		roots, err := d.Store.TrashedRoots(ctx)
		if err != nil {
			return nil, FromStoreError(err)
		}
		out := &trashListOutput{}
		out.Body.Items = []Node{}
		for _, n := range roots {
			out.Body.Items = append(out.Body.Items, fromStoreNode(n))
		}
		return out, nil
	})

	type emptyOutput struct{ Body TrashEmptyReport }
	huma.Register(api, huma.Operation{
		OperationID: "emptyTrash", Method: http.MethodPost, Path: "/api/v1/trash/empty",
		Summary: "Report (run=false) or hard-delete (run=true) trash roots",
	}, func(ctx context.Context, in *struct {
		Body struct {
			OlderThan string `json:"older_than,omitempty" example:"30d"`
			Run       bool   `json:"run,omitempty" default:"false"`
		}
	}) (*emptyOutput, error) {
		age, err := ParseAge(in.Body.OlderThan)
		if err != nil {
			return nil, NewError(http.StatusUnprocessableEntity, "validation", err.Error())
		}
		out := &emptyOutput{}
		err = g.maintain(func() error {
			rep, err := d.Store.TrashEmpty(ctx, age, in.Body.Run)
			if err != nil {
				return FromStoreError(err)
			}
			out.Body = TrashEmptyReport{
				CandidateRoots: rep.Candidates,
				Deleted:        rep.Deleted,
				Run:            rep.Run,
			}
			return nil
		})
		return out, err
	})

	type gcOutput struct{ Body GCReport }
	huma.Register(api, huma.Operation{
		OperationID: "gc", Method: http.MethodPost, Path: "/api/v1/gc",
		Summary: "Report (run=false) or reclaim (run=true) unreachable blobs",
	}, func(ctx context.Context, in *struct {
		Body struct {
			Run bool `json:"run"`
		}
	}) (*gcOutput, error) {
		out := &gcOutput{}
		err := g.maintain(func() error {
			return d.Blobs.WithMutation(ctx, func() error {
				rep, err := runGC(ctx, d, in.Body.Run)
				if err != nil {
					return err
				}
				out.Body = rep
				return nil
			})
		})
		return out, err
	})

	type verifyOutput struct{ Body VerifyReport }
	huma.Register(api, huma.Operation{
		OperationID: "verify", Method: http.MethodPost, Path: "/api/v1/verify",
		Summary: "Re-hash every stored blob and report corruption",
	}, func(ctx context.Context, _ *struct{}) (*verifyOutput, error) {
		out := &verifyOutput{}
		err := g.maintain(func() error {
			blobs, err := d.Store.AllBlobs(ctx)
			if err != nil {
				return FromStoreError(err)
			}
			for _, b := range blobs {
				if err := ctx.Err(); err != nil {
					return NewError(http.StatusInternalServerError, "internal",
						fmt.Sprintf("verify interrupted: %v", err))
				}
				if problem := checkBlob(ctx, d, b.Hash); problem == "" {
					out.Body.OK++
				} else {
					out.Body.Problems = append(out.Body.Problems, VerifyProblem{Hash: b.Hash, Problem: problem})
				}
			}
			return nil
		})
		return out, err
	})
}

func storagePackReport(stats packstore.PackStats) StoragePackReport {
	return StoragePackReport{
		PacksSealed: stats.PacksSealed, BlobsPacked: stats.BlobsPacked, BytesPacked: stats.BytesPacked,
		PacksAdopted: stats.PacksAdopted, PacksRemoved: stats.PacksRemoved,
		PacksQuarantined: stats.PacksQuarantined, PacksUnreadable: stats.PacksUnreadable,
		RecordsDropped: stats.RecordsDropped, MappingsPruned: stats.MappingsPruned,
		BlobsMissing: stats.BlobsMissing, BlobsCorrupt: stats.BlobsCorrupt,
		BlobsDeferredOversized: stats.BlobsDeferredOversized,
		PacksDeferredOversized: stats.PacksDeferredOversized,
		LooseSwept:             stats.LooseSwept, LooseOrphansRemoved: stats.LooseOrphansRemoved,
		LooseOrphanSweepSuppressed: stats.LooseOrphanSweepSuppressed,
		BudgetExhausted:            stats.BudgetExhausted,
	}
}

func storageRepackReport(stats packstore.RepackStats) StorageRepackReport {
	return StorageRepackReport{
		MappingsPruned: stats.MappingsPruned, PacksSelected: stats.PacksSelected,
		PacksRewritten: stats.PacksRewritten, PacksSealed: stats.PacksSealed,
		PacksRemoved: stats.PacksRemoved, PacksDeferredOversized: stats.PacksDeferredOversized,
		BlobsRepacked: stats.BlobsRepacked, BytesRepacked: stats.BytesRepacked,
		BudgetExhausted: stats.BudgetExhausted,
	}
}

func storageUnpackReport(stats packstore.UnpackStats) StorageUnpackReport {
	return StorageUnpackReport{
		PacksUnpacked: stats.PacksUnpacked, BlobsRestored: stats.BlobsRestored,
		BytesRestored: stats.BytesRestored, MappingsPruned: stats.MappingsPruned,
	}
}

// runGC ports cmd/gc.go's semantics: candidates from row reachability,
// untracked files from a shard scan (safe under the maintenance gate — no
// concurrent ingest can be mid-write), files removed before rows so a crash
// leaves reconcilable row-without-file state, never the reverse.
func runGC(ctx context.Context, d Deps, run bool) (GCReport, error) {
	candidates, err := d.Store.UnreachableBlobs(ctx)
	if err != nil {
		return GCReport{}, FromStoreError(err)
	}
	tracked, err := d.Store.AllBlobs(ctx)
	if err != nil {
		return GCReport{}, FromStoreError(err)
	}
	trackedSet := make(map[string]bool, len(tracked))
	for _, b := range tracked {
		trackedSet[b.Hash] = true
	}
	files, err := d.Blobs.List()
	if err != nil {
		return GCReport{}, FromStoreError(err)
	}
	var untracked []string
	rep := GCReport{CandidateBlobs: len(candidates), Run: run}
	for hash, size := range files {
		if !trackedSet[hash] {
			untracked = append(untracked, hash)
			rep.ReclaimableBytes += size
		}
	}
	sort.Strings(untracked)
	rep.UntrackedFiles = len(untracked)
	packedSizes, err := d.Store.PackedBlobStoredBytes(ctx)
	if err != nil {
		return GCReport{}, FromStoreError(err)
	}
	for _, c := range candidates {
		if looseSize, exists := files[c.Hash]; exists {
			rep.ReclaimableBytes += looseSize
		}
		if storedSize, packed := packedSizes[c.Hash]; packed {
			rep.PendingPackedBlobs++
			rep.PendingPackedBytes += storedSize
		}
	}
	if !run {
		return rep, nil
	}
	for _, h := range untracked {
		if err := d.Blobs.Remove(h); err != nil {
			return GCReport{}, FromStoreError(err)
		}
	}
	hashes := make([]string, 0, len(candidates))
	for _, c := range candidates {
		if err := d.Blobs.Remove(c.Hash); err != nil {
			return GCReport{}, FromStoreError(err)
		}
		hashes = append(hashes, c.Hash)
	}
	if err := d.Store.DeleteBlobRows(ctx, hashes); err != nil {
		return GCReport{}, FromStoreError(err)
	}
	rep.ReclaimedFiles = len(untracked)
	for _, c := range candidates {
		if _, existed := files[c.Hash]; existed {
			rep.ReclaimedFiles++
		}
	}
	rep.RemovedBlobs = len(hashes)
	rep.Removed = len(hashes) + len(untracked)
	return rep, nil
}

// checkBlob returns "", "missing", "corrupt", or "unreadable".
func checkBlob(ctx context.Context, d Deps, hash string) string {
	f, err := d.Blobs.OpenContext(ctx, hash)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return "missing"
		}
		return "unreadable"
	}
	defer func() { _ = f.Close() }()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "unreadable"
	}
	if hex.EncodeToString(h.Sum(nil)) != hash {
		return "corrupt"
	}
	return ""
}
