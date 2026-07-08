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

	"github.com/danielgtaylor/huma/v2"

	"go.kenn.io/docbank/internal/ingest"
)

func registerOpsRoutes(api huma.API, d Deps, g *gate) {
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
		out := &ingestOutput{}
		err := g.mutate(func() error {
			ing := &ingest.Ingester{Store: d.Store, Blobs: d.Blobs}
			rep, err := ing.AddPaths(ctx, in.Body.Paths, in.Body.Dest)
			if err != nil {
				return FromStoreError(err)
			}
			out.Body = IngestReport{Added: rep.Added, Skipped: rep.Skipped}
			for _, f := range rep.Failed {
				out.Body.Failed = append(out.Body.Failed, IngestFailure{Path: f.Path, Error: f.Err.Error()})
			}
			return nil
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

	type emptyOutput struct {
		Body struct {
			Deleted int64 `json:"deleted"`
		}
	}
	huma.Register(api, huma.Operation{
		OperationID: "emptyTrash", Method: http.MethodPost, Path: "/api/v1/trash/empty",
		Summary: "Hard-delete trash roots (their blobs become gc candidates)",
	}, func(ctx context.Context, in *struct {
		Body struct {
			OlderThan string `json:"older_than,omitempty" example:"30d"`
		}
	}) (*emptyOutput, error) {
		age, err := ParseAge(in.Body.OlderThan)
		if err != nil {
			return nil, NewError(http.StatusUnprocessableEntity, "validation", err.Error())
		}
		out := &emptyOutput{}
		err = g.maintain(func() error {
			n, err := d.Store.EmptyTrash(ctx, age)
			if err != nil {
				return FromStoreError(err)
			}
			out.Body.Deleted = n
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
			rep, err := runGC(ctx, d, in.Body.Run)
			if err != nil {
				return err
			}
			out.Body = rep
			return nil
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
				if problem := checkBlob(d, b.Hash); problem == "" {
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
	for _, c := range candidates {
		rep.ReclaimableBytes += c.Size
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
	rep.Removed = len(hashes) + len(untracked)
	return rep, nil
}

// checkBlob returns "", "missing", "corrupt", or "unreadable".
func checkBlob(d Deps, hash string) string {
	f, err := d.Blobs.Open(hash)
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
