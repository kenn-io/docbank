package processing

import (
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/json/v2"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/internal/config"
	"go.kenn.io/docbank/internal/store"
)

const localTextSkipLimit = 262_144

type localTextSkipKey struct{ version, name string }
type localTextSkip struct{ seen bool }

type LocalTextPlan struct {
	Fingerprint        string    `json:"fingerprint"`
	VaultUID           string    `json:"vault_uid"`
	Profile            string    `json:"profile"`
	Server             string    `json:"server"`
	ProfileFingerprint string    `json:"profile_fingerprint"`
	Models             []string  `json:"models"`
	VectorEffect       string    `json:"vector_effect"`
	Managed            bool      `json:"managed"`
	Flow               []FlowHop `json:"flow"`
	RetainedClasses    []string  `json:"retained_classes"`
	Scope              string    `json:"scope"`
}

// LocalTextSetup builds a private service for the reviewed profile. Activation
// happens on daemon restart, so all consumers keep one effective identity.
type LocalTextSetup struct {
	Service       *Service
	Candidate     func(string) (*Service, error)
	Selected      string
	Startup       func(context.Context, bool, string) error
	Save          func(string, string) error
	ActivityBegin func()
	ActivityEnd   func()
	mu            sync.Mutex
	// mu also guards the skip cache during the daemon's final idle check.
	skips      map[localTextSkipKey]*localTextSkip
	skipWarned bool
	logger     *slog.Logger
}

func (setup *LocalTextSetup) review(ctx context.Context, mode string) (LocalTextPlan, *Service, error) {
	name := config.LocalTextProfile
	service, err := setup.Candidate(mode)
	if err != nil {
		return LocalTextPlan{}, nil, err
	}
	keep := false
	defer func() {
		if !keep {
			service.Stop()
		}
	}()
	profile, ok := service.profiles[name]
	if !ok {
		return LocalTextPlan{}, nil, ErrProfileNotConfigured
	}
	operation, err := service.planForSource(ctx, Selector{Profile: name}, store.Node{}, store.ContentVersion{}, profile)
	if err != nil {
		return LocalTextPlan{}, nil, err
	}
	plan := LocalTextPlan{VaultUID: service.catalog.VaultID(), Profile: name, Server: mode, ProfileFingerprint: profile.record.Fingerprint,
		Managed: true, Flow: operation.Flow, RetainedClasses: operation.RetainedClasses, Scope: "this vault and new imports",
		VectorEffect: "builds the separate local-text index; embeds inputs it is missing"}
	for _, binding := range profile.portable.Embeddings {
		plan.Models = append(plan.Models, binding.Model)
	}
	encoded, err := json.Marshal(plan, json.Deterministic(true))
	if err != nil {
		return LocalTextPlan{}, nil, err
	}
	plan.Fingerprint = fmt.Sprintf("%x", sha256.Sum256(encoded))
	keep = true
	return plan, service, nil
}

func (setup *LocalTextSetup) Plan(ctx context.Context, mode string) (LocalTextPlan, error) {
	plan, service, err := setup.review(ctx, mode)
	if service != nil {
		service.Stop()
	}
	return plan, err
}

func (setup *LocalTextSetup) Apply(ctx context.Context, mode, fingerprint string) (LocalTextPlan, error) {
	setup.mu.Lock()
	defer setup.mu.Unlock()
	plan, service, err := setup.review(ctx, mode)
	if err != nil {
		return LocalTextPlan{}, err
	}
	defer service.Stop()
	if fingerprint == "" || plan.Fingerprint != fingerprint {
		return LocalTextPlan{}, ErrPlanChanged
	}
	if err := setup.Startup(ctx, plan.Managed, mode); err != nil {
		return LocalTextPlan{}, err
	}
	if err := service.grantProfileConsent(ctx, service.profiles[plan.Profile], nil); err != nil {
		return LocalTextPlan{}, err
	}
	if err := setup.Service.gate.MutateContext(ctx, func() error { return setup.Save(plan.Profile, plan.Server) }); err != nil {
		return LocalTextPlan{}, fmt.Errorf("%w: save local text selection: %w", ErrConsentSaved, err)
	}

	return plan, nil
}

func (setup *LocalTextSetup) targets(ctx context.Context, after string, limit int) ([]store.DocumentSummary, error) {
	setup.mu.Lock()
	defer setup.mu.Unlock()
	return setup.listTargets(ctx, after, limit, true)
}

func (setup *LocalTextSetup) listTargets(ctx context.Context, after string, limit int, sweep bool) ([]store.DocumentSummary, error) {
	profile, ok := setup.Service.profiles[setup.Selected]
	if !ok {
		return nil, nil
	}
	for _, request := range setup.Service.profileConsentRequests(profile) {
		_, err := setup.Service.catalog.AuthorizeProviderOperation(ctx, request)
		state, err := processingConsentState(err)
		if err != nil {
			return nil, err
		}
		if state != "active" {
			return nil, nil
		}
	}
	var result []store.DocumentSummary
	for len(result) < limit {
		items, err := setup.Service.catalog.MissingProcessingTargetsAfter(ctx, profile.record.Fingerprint, after, len(profile.portable.Embeddings), 1000)
		if err != nil {
			return nil, err
		}
		keys := make([]store.EmbeddingHeadKey, 0, len(items)*len(profile.portable.Embeddings))
		for _, item := range items {
			for _, binding := range profile.portable.Embeddings {
				keys = append(keys, store.EmbeddingHeadKey{ContentVersionID: item.ContentVersionID, BindingID: binding.Name, InputKind: binding.InputKind})
			}
		}
		suppressed, err := setup.Service.catalog.EmbeddingPurgeSuppressedVersions(ctx, profile.record.Fingerprint, keys)
		if err != nil {
			return nil, err
		}
		for _, item := range items {
			if suppressed[item.ContentVersionID] {
				continue
			}
			key := localTextSkipKey{item.ContentVersionID, item.Name}
			if skip, ok := setup.skips[key]; ok {
				if sweep {
					skip.seen = true
				}
				continue
			}
			result = append(result, item)
			if len(result) == limit {
				return result, nil
			}
		}
		if len(items) < 1000 {
			if len(result) == 0 && sweep {
				for key, skip := range setup.skips {
					if !skip.seen {
						delete(setup.skips, key)
					} else {
						skip.seen = false
					}
				}
			}
			return result, nil
		}
		after = items[len(items)-1].ContentVersionID
	}
	return result, nil
}

func (setup *LocalTextSetup) process(ctx context.Context, item store.DocumentSummary) error {
	selector := Selector{Profile: setup.Selected, NodeID: item.NodeID, ContentVersionID: item.ContentVersionID}
	profile := setup.Service.profiles[setup.Selected]
	plan, err := setup.Service.Plan(ctx, selector)
	if err != nil {
		return err
	}
	if plan.RenditionIneligibleReason != "" {
		if profileRenditionInput(profile) == document.RenditionInputOriginalFile {
			setup.skip(item, "")
		}
		return nil
	}
	job, err := setup.Service.Start(ctx, StartRequest{Selector: selector, PlanFingerprint: plan.Fingerprint,
		runOptions: processingRunOptions{retryUnavailable: true, deferIndex: true}})
	if errors.Is(err, document.ErrTokenizerLimit) {
		setup.skip(item, "tokenizer_limit")
		return nil
	}
	if errors.Is(err, ErrRenditionFailed) {
		setup.skip(item, "rendition_failed")
		return nil
	}
	if errors.Is(err, store.ErrNotFound) || errors.Is(err, ErrPlanChanged) {
		return nil
	}
	if err != nil {
		return err
	}
	for _, id := range job.EmbeddingJobIDs {
		status, statusErr := setup.Service.catalog.EmbeddingJobByID(ctx, id)
		if statusErr != nil {
			return statusErr
		}
		if status.State == "failed" {
			switch status.FailureCode {
			case store.EmbeddingFailureInvalidResponse, store.EmbeddingFailureInputRejected, store.EmbeddingFailureStaleAuthority:
				setup.skip(item, string(status.FailureCode))
			default:
				err = errors.Join(err, fmt.Errorf("local text embedding job %s failed: %s", id, status.FailureCode))
			}
		}
	}
	return err
}

func (setup *LocalTextSetup) skip(item store.DocumentSummary, code string) {
	setup.mu.Lock()
	defer setup.mu.Unlock()
	logger := cmp.Or(setup.logger, slog.Default())
	if setup.skips == nil {
		setup.skips = make(map[localTextSkipKey]*localTextSkip)
	}
	key := localTextSkipKey{item.ContentVersionID, item.Name}
	if _, exists := setup.skips[key]; exists {
		return
	}
	if len(setup.skips) < localTextSkipLimit {
		setup.skips[key] = &localTextSkip{seen: true}
		if code != "" {
			logger.Warn("local text document skipped", "version", item.ContentVersionID, "code", code)
		}
	} else if !setup.skipWarned {
		logger.Warn("local text skip limit reached; additional ineligible files will be inspected again", "limit", localTextSkipLimit)
		setup.skipWarned = true
	}
}

// Pending checks current work before idle shutdown, including imports admitted
// between lane polls. It leaves the lane's sweep/pruning bookkeeping intact.
func (setup *LocalTextSetup) Pending(ctx context.Context) (bool, error) {
	setup.mu.Lock()
	defer setup.mu.Unlock()
	items, err := setup.listTargets(ctx, "", 1, false)
	if err != nil {
		return false, err
	}
	if len(items) > 0 {
		return true, nil
	}
	return setup.publicationPending(ctx)
}

// publicationPending compares the publisher's active manifest with current
// membership. The recurring daemon index worker owns the rebuild itself.
func (setup *LocalTextSetup) publicationPending(ctx context.Context) (bool, error) {
	profile, ok := setup.Service.profiles[setup.Selected]
	if !ok {
		return false, nil
	}
	for _, space := range profile.vectorSpaces {
		pending, err := setup.Service.catalog.VectorIndexPublicationPending(ctx, space)
		if err != nil || pending {
			return pending, err
		}
	}
	return false, nil
}

func (setup *LocalTextSetup) Backfill(ctx context.Context, logger *slog.Logger) error {
	setup.logger = logger
	holding := false
	release := func() {
		if holding {
			holding = false
			if setup.ActivityEnd != nil {
				setup.ActivityEnd()
			}
		}
	}
	defer release()
	lane := Backfill[store.DocumentSummary]{Name: "local-text", Page: 50, IdleDelay: time.Second, Logger: logger,
		List: func(ctx context.Context, after string, limit int) ([]store.DocumentSummary, error) {
			items, err := setup.targets(ctx, after, limit)
			pending := len(items) > 0
			if err == nil && after == "" && !pending {
				pending, err = setup.publicationPending(ctx)
			}
			if pending && !holding {
				holding = true
				if setup.ActivityBegin != nil {
					setup.ActivityBegin()
				}
			} else if err == nil && after == "" && !pending {
				release()
			}
			return items, err
		}, Key: func(item store.DocumentSummary) string { return item.ContentVersionID }, Process: setup.process}
	return lane.Run(ctx)
}
