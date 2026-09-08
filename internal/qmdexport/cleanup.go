package qmdexport

import (
	"context"
	"errors"
	"os"
)

const (
	candidateMaximum    = 4096
	cleanupEntryMaximum = 1_000_000
	cleanupByteMaximum  = int64(16 << 30)
)

var (
	errCleanupBound      = errors.New("qmd export cleanup exceeds bound")
	errCleanupIncomplete = errors.New("qmd export cleanup is incomplete")
)

type cleanupBudget struct {
	entries int
	bytes   int64
}

func newCleanupBudget() *cleanupBudget {
	return &cleanupBudget{entries: cleanupEntryMaximum, bytes: cleanupByteMaximum}
}

// entries may observe maximum+1 entries to establish overflow. Reserve that
// observation before starting, including when the shared budget is nearly empty.
func enumerateCleanupEntries(ctx context.Context, dir *anchoredDir, maximum int, budget *cleanupBudget) ([]os.DirEntry, error) {
	allowance := min(maximum+1, budget.entries)
	if allowance < 1 {
		return nil, errCleanupBound
	}
	budget.entries -= allowance
	entries, err := dir.entries(ctx, allowance-1)
	if err != nil {
		if errors.Is(err, errDirectoryBound) && allowance <= maximum {
			return nil, errCleanupBound
		}
		return nil, err
	}
	budget.entries += allowance - len(entries)
	return entries, nil
}

func cleanupOwnedGenerations(ctx context.Context, root *ownedRoot, selected string, bounds Options) (CleanupStatus, error) {
	return cleanupWithBudget(ctx, root, selected, bounds, newCleanupBudget(), candidateMaximum, nil)
}

func cleanupWithBudget(ctx context.Context, root *ownedRoot, selected string, bounds Options, budget *cleanupBudget, candidateLimit int, beforeRemove func()) (status CleanupStatus, retErr error) {
	rootEntries, err := enumerateCleanupEntries(ctx, root.root, rootEntryMaximum, budget)
	if err != nil {
		status.ScanLimitReached = errors.Is(err, errDirectoryBound) || errors.Is(err, errCleanupBound)
		return status, err
	}
	for _, entry := range rootEntries {
		switch entry.Name() {
		case markerName, lockName, "CURRENT", "generations", ".staging":
		default:
			status.UnknownEntries++
		}
	}
	if status.UnknownEntries != 0 {
		retErr = errCleanupIncomplete
	}
	// Each managed parent is enumerated once. Overflow does not invent counts
	// for candidates that have never been observed.
	for _, parent := range []*anchoredDir{root.generations, root.staging} {
		candidates, err := enumerateCleanupEntries(ctx, parent, candidateLimit, budget)
		if err != nil {
			status.ScanLimitReached = status.ScanLimitReached || errors.Is(err, errDirectoryBound) || errors.Is(err, errCleanupBound)
			retErr = errors.Join(retErr, err)
			return status, retErr
		}
		for _, candidate := range candidates {
			if parent == root.generations && candidate.Name() == selected {
				continue
			}
			if err := ctx.Err(); err != nil {
				return status, errors.Join(retErr, err)
			}
			id := ""
			if parent == root.generations {
				id = candidate.Name()
			}
			var verified *verifiedGeneration
			if !candidate.IsDir() || (parent == root.generations && !validChecksum(id)) {
				err = errGenerationInvalid
			} else {
				verified, err = verifyOwnedGeneration(ctx, root, parent, candidate.Name(), id, bounds, budget)
			}
			if err == nil {
				if beforeRemove != nil {
					beforeRemove()
				}
				err = verified.ledger.remove(ctx)
				err = errors.Join(err, verified.ledger.close())
			}
			if err != nil {
				status.PreservedCandidates++
				status.ScanLimitReached = status.ScanLimitReached || errors.Is(err, errCleanupBound)
				retErr = errors.Join(retErr, err)
				if errors.Is(err, errCleanupBound) || ctx.Err() != nil {
					return status, retErr
				}
			} else {
				status.RemovedGenerations++
			}
		}
	}
	return status, errors.Join(retErr, ctx.Err())
}
