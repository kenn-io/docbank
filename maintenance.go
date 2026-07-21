package docbank

import (
	"context"
	"time"

	internalmaintenance "go.kenn.io/docbank/internal/maintenance"
)

const (
	DefaultMaintenanceMaxObjects = internalmaintenance.DefaultMaxObjects
	MaxMaintenanceObjects        = internalmaintenance.MaxObjectsPerOperation
)

var ErrInvalidMaintenanceCursor = internalmaintenance.ErrInvalidCursor

// WorkBudget bounds one embedded maintenance pass. MaxObjects zero uses
// DefaultMaintenanceMaxObjects and values above MaxMaintenanceObjects are
// rejected. MaxBytes zero is unlimited; a positive byte limit is soft, so one
// selected object may carry the pass over the limit.
type WorkBudget struct {
	MaxObjects int
	MaxBytes   int64
	Cursor     string
}

type GCOptions struct {
	Budget WorkBudget
	DryRun bool
}

type VerifyOptions struct{ Budget WorkBudget }

type RepackOptions struct {
	Budget       WorkBudget
	MinAge       time.Duration
	MinDeadBytes int64
}

type MaintenanceProgress struct {
	NextCursor string `json:"next_cursor,omitempty"`
	More       bool   `json:"more"`
}

// GCReport summarizes one bounded catalog-authority reclamation pass.
type GCReport struct {
	MaintenanceProgress

	CandidateBlobs     int   `json:"candidate_blobs"`
	UntrackedFiles     int   `json:"untracked_files"`
	ReclaimableBytes   int64 `json:"reclaimable_bytes"`
	PendingPackedBlobs int   `json:"pending_packed_blobs"`
	PendingPackedBytes int64 `json:"pending_packed_bytes"`
	ReclaimedFiles     int   `json:"reclaimed_files"`
	RemovedBlobs       int   `json:"removed_blobs"`
	Removed            int   `json:"removed"`
	DryRun             bool  `json:"dry_run"`
}

type VerifyProblem struct {
	Hash    string `json:"hash"`
	Problem string `json:"problem"`
}

// VerifyReport summarizes one bounded content verification pass. The metadata
// field remains for compatibility with the daemon's full verification report.
type VerifyReport struct {
	MaintenanceProgress

	OK               int             `json:"ok"`
	Problems         []VerifyProblem `json:"problems,omitempty"`
	MetadataProblems []string        `json:"metadata_problems,omitempty"`
}

// RepackReport summarizes one bounded immutable-pack reclamation pass.
type RepackReport struct {
	MaintenanceProgress

	MappingsPruned         int64 `json:"mappings_pruned"`
	PacksSelected          int   `json:"packs_selected"`
	PacksRewritten         int   `json:"packs_rewritten"`
	PacksSealed            int   `json:"packs_sealed"`
	PacksRemoved           int   `json:"packs_removed"`
	PacksDeferredOversized int   `json:"packs_deferred_oversized"`
	BlobsRepacked          int   `json:"blobs_repacked"`
	BytesRepacked          int64 `json:"bytes_repacked"`
	BudgetExhausted        bool  `json:"budget_exhausted"`
}

// GarbageCollect previews or removes one bounded canonical-hash page of
// unreachable catalog authority. The daemon separately reconciles physical
// orphan files for its legacy full-maintenance endpoint.
func (v *Vault) GarbageCollect(ctx context.Context, opts GCOptions) (GCReport, error) {
	if err := v.begin(); err != nil {
		return GCReport{}, err
	}
	defer v.lifecycle.RUnlock()
	v.mutation.Lock()
	defer v.mutation.Unlock()
	var report internalmaintenance.GCReport
	err := v.blobs.WithMutation(ctx, func() error {
		var err error
		report, err = internalmaintenance.GarbageCollect(ctx, v.metadata, v.blobs,
			internalmaintenance.GCOptions{Budget: internalBudget(opts.Budget), DryRun: opts.DryRun})
		return err
	})
	return fromMaintenanceGC(report), err
}

// Verify validates one bounded canonical-hash page of catalog-authorized
// content. Whole-catalog metadata validation remains daemon-only.
func (v *Vault) Verify(ctx context.Context, opts VerifyOptions) (VerifyReport, error) {
	if err := v.begin(); err != nil {
		return VerifyReport{}, err
	}
	defer v.lifecycle.RUnlock()
	v.mutation.Lock()
	defer v.mutation.Unlock()
	report, err := internalmaintenance.Verify(ctx, v.metadata, v.blobs,
		internalmaintenance.VerifyOptions{Budget: internalBudget(opts.Budget)})
	return fromMaintenanceVerify(report), err
}

// Repack retires dead packs and rewrites eligible sparse packs within one
// bounded pass while preserving Kit's soft raw-byte budget.
func (v *Vault) Repack(ctx context.Context, opts RepackOptions) (RepackReport, error) {
	if err := v.begin(); err != nil {
		return RepackReport{}, err
	}
	defer v.lifecycle.RUnlock()
	v.mutation.Lock()
	defer v.mutation.Unlock()
	report, err := internalmaintenance.Repack(ctx, v.metadata, v.blobs,
		internalmaintenance.RepackOptions{
			Budget: internalBudget(opts.Budget), MinAge: opts.MinAge, MinDeadBytes: opts.MinDeadBytes,
		})
	return fromMaintenanceRepack(report), err
}

func internalBudget(budget WorkBudget) internalmaintenance.Budget {
	return internalmaintenance.Budget{
		MaxObjects: budget.MaxObjects, MaxBytes: budget.MaxBytes, Cursor: budget.Cursor,
	}
}

func fromMaintenanceGC(report internalmaintenance.GCReport) GCReport {
	return GCReport{
		MaintenanceProgress: MaintenanceProgress{
			NextCursor: report.NextCursor, More: report.More,
		},
		CandidateBlobs: report.CandidateBlobs, UntrackedFiles: report.UntrackedFiles,
		ReclaimableBytes:   report.ReclaimableBytes,
		PendingPackedBlobs: report.PendingPackedBlobs,
		PendingPackedBytes: report.PendingPackedBytes, ReclaimedFiles: report.ReclaimedFiles,
		RemovedBlobs: report.RemovedBlobs, Removed: report.Removed, DryRun: report.DryRun,
	}
}

func fromMaintenanceVerify(report internalmaintenance.VerifyReport) VerifyReport {
	out := VerifyReport{MaintenanceProgress: MaintenanceProgress{
		NextCursor: report.NextCursor, More: report.More,
	}, OK: report.OK, MetadataProblems: report.MetadataProblems}
	for _, problem := range report.Problems {
		out.Problems = append(out.Problems, VerifyProblem{Hash: problem.Hash, Problem: problem.Problem})
	}
	return out
}

func fromMaintenanceRepack(report internalmaintenance.RepackReport) RepackReport {
	return RepackReport{
		MaintenanceProgress: MaintenanceProgress{
			NextCursor: report.NextCursor, More: report.More,
		},
		MappingsPruned: report.MappingsPruned, PacksSelected: report.PacksSelected,
		PacksRewritten: report.PacksRewritten, PacksSealed: report.PacksSealed,
		PacksRemoved:           report.PacksRemoved,
		PacksDeferredOversized: report.PacksDeferredOversized,
		BlobsRepacked:          report.BlobsRepacked, BytesRepacked: report.BytesRepacked,
		BudgetExhausted: report.BudgetExhausted,
	}
}
