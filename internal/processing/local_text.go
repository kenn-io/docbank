package processing

import (
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/json/v2"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"slices"
	"strings"
	"sync"
	"time"

	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/internal/config"
	"go.kenn.io/docbank/internal/store"
)

const localTextSkipLimit = 262_144

type localTextSkipKey struct{ version, name string }
type localTextSkip struct{ seen bool }

type LocalTextRetainedError struct{ Reasons []string }

func (e *LocalTextRetainedError) Error() string { return strings.Join(e.Reasons, "; ") }

type LocalTextTarget struct {
	Server              string `json:"server"`
	EmbeddingServer     string `json:"embedding_server"`
	EmbeddingModel      string `json:"embedding_model"`
	EmbeddingDimensions int    `json:"embedding_dimensions"`
}

type LocalTextProfilePlan struct {
	Name        string `json:"name"`
	Fingerprint string `json:"fingerprint"`
}

type LocalTextPlan struct {
	Fingerprint        string                 `json:"fingerprint"`
	VaultUID           string                 `json:"vault_uid"`
	Profile            string                 `json:"profile"`
	Server             string                 `json:"server"`
	ProfileFingerprint string                 `json:"profile_fingerprint"`
	Models             []string               `json:"models"`
	VectorEffect       string                 `json:"vector_effect"`
	Managed            bool                   `json:"managed"`
	Flow               []FlowHop              `json:"flow"`
	RetainedClasses    []string               `json:"retained_classes"`
	Scope              string                 `json:"scope"`
	Target             *LocalTextTarget       `json:"target,omitempty"`
	Profiles           []LocalTextProfilePlan `json:"profiles,omitempty"`
	Retained           []string               `json:"retained,omitempty"`
}

// LocalTextSetup builds a private service for the reviewed profile. Activation
// happens on daemon restart, so all consumers keep one effective identity.
type LocalTextSetup struct {
	Service          *Service
	Candidate        func(mode, name string) (*Service, error)
	CandidateTarget  func(LocalTextTarget, string) (*Service, []string, error)
	Selected         string
	SelectedProfiles []string
	Startup          func(context.Context, bool, string) error
	Save             func(string, string) error
	SaveTarget       func([]string, LocalTextTarget) error
	ActivityBegin    func()
	ActivityEnd      func()
	mu               sync.Mutex
	// mu also guards the skip cache during the daemon's final idle check.
	skips        map[localTextSkipKey]*localTextSkip
	skipWarned   bool
	logger       *slog.Logger
	profileLanes map[string]*LocalTextSetup
}

func (setup *LocalTextSetup) review(ctx context.Context, mode string) (LocalTextPlan, *Service, error) {
	name, effect := "", "local-text (separate; adds missing content)"
	var service *Service
	names := slices.Sorted(maps.Keys(setup.Service.profiles))
	if setup.Selected != "" && setup.Selected != config.LocalTextProfile {
		names = append([]string{setup.Selected}, slices.DeleteFunc(names, func(name string) bool { return name == setup.Selected })...)
	}
	for _, candidate := range names {
		if candidate == config.LocalTextProfile {
			continue
		}
		indexed, err := setup.indexedText(ctx, candidate)
		if err != nil {
			return LocalTextPlan{}, nil, err
		}
		if !indexed && candidate != setup.Selected {
			continue
		}
		trial, err := setup.Candidate(mode, candidate)
		if errors.Is(err, ErrProfileNotConfigured) {
			if indexed && setup.Selected != config.LocalTextProfile {
				effect = "Adds separate index"
			}
			continue
		}
		if err != nil {
			return LocalTextPlan{}, nil, err
		}
		name, effect, service = candidate, "Keep existing index", trial
		if setup.Selected == config.LocalTextProfile {
			effect += "; replaces local-text for new imports"
		}
		break
	}
	if service == nil {
		name = config.LocalTextProfile
		var err error
		service, err = setup.Candidate(mode, name)
		if err != nil {
			return LocalTextPlan{}, nil, err
		}
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
		VectorEffect: effect}
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

func (setup *LocalTextSetup) indexedText(ctx context.Context, name string) (bool, error) {
	active := setup.Service.profiles[name]
	for _, binding := range active.portable.Embeddings {
		if binding.InputKind != document.EmbeddingInputRenditionChunk {
			continue
		}
		exists, err := setup.Service.catalog.HasEmbeddingHeads(ctx, active.record.Fingerprint, binding.Name)
		if err != nil {
			return false, err
		}
		if exists {
			return true, nil
		}
	}
	return false, nil
}

// reviewTarget prepares each affected recipe before approving or saving the choice.
func (setup *LocalTextSetup) reviewTarget(ctx context.Context, target LocalTextTarget) (plan LocalTextPlan, services map[string]*Service, err error) {
	if target.EmbeddingServer == "" || target.EmbeddingModel == "" || target.EmbeddingDimensions <= 0 || setup.CandidateTarget == nil {
		return plan, nil, ErrProfileNotConfigured
	}
	services = make(map[string]*Service)
	defer func() {
		if err != nil {
			for _, service := range services {
				service.Stop()
			}
			services = nil
		}
	}()
	plan = LocalTextPlan{VaultUID: setup.Service.catalog.VaultID(), Server: target.Server, Target: &target,
		Scope: "this vault and new imports", VectorEffect: "Rebuild affected indexes"}
	names := slices.Sorted(maps.Keys(setup.Service.profiles))
	fallback := !slices.Contains(names, config.LocalTextProfile)
	if fallback {
		names = append(names, config.LocalTextProfile)
	}
	for _, name := range names {
		if fallback && name == config.LocalTextProfile && (len(services) > 0 || len(plan.Retained) > 0) {
			continue
		}
		service, retained, candidateErr := setup.CandidateTarget(target, name)
		plan.Retained = append(plan.Retained, retained...)
		if errors.Is(candidateErr, ErrProfileNotConfigured) {
			continue
		}
		if candidateErr != nil {
			return plan, services, candidateErr
		}
		if service == nil {
			return plan, services, ErrProfileNotConfigured
		}
		services[name] = service
		profile, ok := service.profiles[name]
		if !ok {
			return plan, services, ErrProfileNotConfigured
		}
		operation, operationErr := service.planForSource(ctx, Selector{Profile: name}, store.Node{}, store.ContentVersion{}, profile)
		if operationErr != nil {
			return plan, services, operationErr
		}
		if plan.Profile == "" {
			plan.Profile, plan.ProfileFingerprint = name, profile.record.Fingerprint
		}
		plan.Profiles = append(plan.Profiles, LocalTextProfilePlan{Name: name, Fingerprint: profile.record.Fingerprint})
		plan.Flow = append(plan.Flow, operation.Flow...)
		plan.RetainedClasses = append(plan.RetainedClasses, operation.RetainedClasses...)
		for _, binding := range profile.portable.Embeddings {
			plan.Models = append(plan.Models, binding.Model)
		}
	}
	if len(services) == 0 {
		if len(plan.Retained) > 0 {
			return plan, services, &LocalTextRetainedError{Reasons: plan.Retained}
		}
		return plan, services, ErrProfileNotConfigured
	}
	count := 0
	for _, profile := range plan.Profiles {
		n, countErr := setup.countTargets(ctx, services[profile.Name], profile.Name)
		if countErr != nil {
			return plan, services, countErr
		}
		count += n
	}
	plan.VectorEffect += fmt.Sprintf("; processes %d documents before setup returns", count)
	encoded, err := json.Marshal(plan, json.Deterministic(true))
	if err != nil {
		return plan, services, err
	}
	plan.Fingerprint = fmt.Sprintf("%x", sha256.Sum256(encoded))
	return plan, services, nil
}

func remoteLocalTextTarget(target LocalTextTarget) bool {
	return target.EmbeddingServer != "" || target.EmbeddingModel != "" || target.EmbeddingDimensions != 0
}

func (setup *LocalTextSetup) reviewChoice(ctx context.Context, target LocalTextTarget) (LocalTextPlan, map[string]*Service, error) {
	if remoteLocalTextTarget(target) {
		return setup.reviewTarget(ctx, target)
	}
	plan, service, err := setup.review(ctx, target.Server)
	if err != nil {
		return plan, nil, err
	}
	if plan.Profile != config.LocalTextProfile {
		count, countErr := setup.countTargets(ctx, service, plan.Profile)
		if countErr != nil {
			service.Stop()
			return plan, nil, countErr
		}
		plan.VectorEffect += fmt.Sprintf("; processes %d documents before setup returns; embeds only new content", count)
	}
	return plan, map[string]*Service{plan.Profile: service}, nil
}

func (setup *LocalTextSetup) PlanTarget(ctx context.Context, target LocalTextTarget) (LocalTextPlan, error) {
	plan, services, err := setup.reviewChoice(ctx, target)
	for _, service := range services {
		service.Stop()
	}
	return plan, err
}

func (setup *LocalTextSetup) ApplyTarget(ctx context.Context, target LocalTextTarget, fingerprint string) (LocalTextPlan, error) {
	setup.mu.Lock()
	defer setup.mu.Unlock()
	plan, services, err := setup.reviewChoice(ctx, target)
	if err != nil {
		return LocalTextPlan{}, err
	}
	defer func() {
		for _, service := range services {
			service.Stop()
		}
	}()
	if fingerprint == "" || plan.Fingerprint != fingerprint {
		return LocalTextPlan{}, ErrPlanChanged
	}
	remote := remoteLocalTextTarget(target)
	if !remote {
		if err := setup.Startup(ctx, plan.Managed, target.Server); err != nil {
			return LocalTextPlan{}, err
		}
	}
	names := []string{plan.Profile}
	if remote {
		if setup.SaveTarget == nil {
			return LocalTextPlan{}, ErrProfileNotConfigured
		}
		names = make([]string, len(plan.Profiles))
		for i, profile := range plan.Profiles {
			names[i] = profile.Name
		}
	}
	for _, name := range names {
		service := services[name]
		if err := service.grantProfileConsent(ctx, service.profiles[name], nil); err != nil {
			return LocalTextPlan{}, err
		}
		if remote || name != config.LocalTextProfile {
			if err := setup.rebuildTarget(ctx, service, name); err != nil {
				return LocalTextPlan{}, fmt.Errorf("%w: activate text indexing selection: %w", ErrConsentSaved, err)
			}
		}
	}
	err = setup.Service.gate.MutateContext(ctx, func() error {
		if remote {
			return setup.SaveTarget(names, target)
		}
		return setup.Save(plan.Profile, target.Server)
	})
	if err != nil {
		return LocalTextPlan{}, fmt.Errorf("%w: save text indexing selection: %w", ErrConsentSaved, err)
	}
	return plan, nil
}

func (setup *LocalTextSetup) rebuildTarget(ctx context.Context, service *Service, name string) error {
	fill := &LocalTextSetup{Service: service, Selected: name, logger: setup.logger}
	pass := Backfill[store.DocumentSummary]{Name: "local-text-move", Page: 50, IdleDelay: time.Second, DrainOnce: true,
		List: fill.targets, Key: func(item store.DocumentSummary) string { return item.ContentVersionID }, Process: fill.process}
	return errors.Join(pass.Run(ctx), service.rebuildVectorSpaces(ctx, slices.Collect(maps.Values(service.profiles[name].vectorSpaces))))
}

func (setup *LocalTextSetup) countTargets(ctx context.Context, service *Service, name string) (int, error) {
	fill := &LocalTextSetup{Service: service, Selected: name}
	count, after := 0, ""
	for {
		items, err := fill.pendingTargets(ctx, after, 1000, false)
		if err != nil {
			return 0, err
		}
		count += len(items)
		if len(items) < 1000 {
			return count, nil
		}
		after = items[len(items)-1].ContentVersionID
	}
}

func (setup *LocalTextSetup) Plan(ctx context.Context, mode string) (LocalTextPlan, error) {
	return setup.PlanTarget(ctx, LocalTextTarget{Server: mode})
}

func (setup *LocalTextSetup) Apply(ctx context.Context, mode, fingerprint string) (LocalTextPlan, error) {
	return setup.ApplyTarget(ctx, LocalTextTarget{Server: mode}, fingerprint)
}

func (setup *LocalTextSetup) targets(ctx context.Context, after string, limit int) ([]store.DocumentSummary, error) {
	setup.mu.Lock()
	defer setup.mu.Unlock()
	return setup.listTargets(ctx, after, limit, true)
}

func (setup *LocalTextSetup) listTargets(ctx context.Context, after string, limit int, sweep bool) ([]store.DocumentSummary, error) {
	eligible, err := setup.Service.ProfileConsentActive(ctx, setup.Selected)
	if err != nil || !eligible {
		return nil, err
	}
	return setup.pendingTargets(ctx, after, limit, sweep)
}

// inheritedConsent checks whether every operation is active or every operation
// is undecided and the vault has no revocation fence.
func (setup *LocalTextSetup) inheritedConsent(ctx context.Context) (bool, bool, error) {
	service := setup.Service
	profile, ok := service.profiles[setup.Selected]
	if !ok {
		return false, false, ErrProfileNotConfigured
	}
	required, active := 0, 0
	for _, request := range service.profileConsentRequests(profile) {
		_, err := service.catalog.AuthorizeProviderOperation(ctx, request)
		state, err := processingConsentState(err)
		if err != nil {
			return false, false, err
		}
		switch state {
		case "required":
			required++
		case "active":
			active++
		default:
			return false, false, nil
		}
	}
	if active != 0 {
		return required == 0, false, nil
	}
	revoked, err := service.catalog.HasConsentRevocation(ctx, service.principal, service.scope)
	return !revoked && err == nil, required != 0, err
}

// InitializeInherited installs the shared runtime before granting undecided consent.
func (setup *LocalTextSetup) InitializeInherited(ctx context.Context) (bool, error) {
	eligible, _, err := setup.inheritedConsent(ctx)
	if err != nil || !eligible {
		return false, err
	}
	if err := setup.inheritedStartup(ctx); err != nil {
		return false, err
	}
	active := false
	err = setup.Service.gate.MutateContext(ctx, func() error {
		eligible, undecided, err := setup.inheritedConsent(ctx)
		if err != nil || !eligible {
			return err
		}
		if undecided {
			active, err = setup.Service.catalog.GrantConsentSetIfUndecided(ctx, setup.Service.profileConsentGrants(setup.Service.profiles[setup.Selected], nil))
			return err
		}
		active = true
		return nil
	})
	return active, err
}

func (setup *LocalTextSetup) inheritedStartup(ctx context.Context) error {
	if setup.ActivityBegin != nil {
		setup.ActivityBegin()
	}
	if setup.ActivityEnd != nil {
		defer setup.ActivityEnd()
	}
	return setup.Startup(ctx, true, "local")
}

// ProfileConsentActive reports whether every operation in a profile has current consent.
func (s *Service) ProfileConsentActive(ctx context.Context, name string) (bool, error) {
	profile, ok := s.profiles[name]
	if !ok {
		return false, nil
	}
	for _, request := range s.profileConsentRequests(profile) {
		_, err := s.catalog.AuthorizeProviderOperation(ctx, request)
		state, err := processingConsentState(err)
		if err != nil {
			return false, err
		}
		if state != "active" {
			return false, nil
		}
	}
	return true, nil
}

func (setup *LocalTextSetup) pendingTargets(ctx context.Context, after string, limit int, sweep bool) ([]store.DocumentSummary, error) {
	profile := setup.Service.profiles[setup.Selected]
	bindings := 0
	for _, binding := range profile.portable.Embeddings {
		if binding.InputKind != document.EmbeddingInputImage {
			bindings++
		}
	}
	// Each pair routes a source to exactly one binding.
	bindings -= len(profile.mediaFallbackBindings)
	var result []store.DocumentSummary
	for len(result) < limit {
		items, err := setup.Service.catalog.MissingProcessingTargetsAfter(ctx, profile.record.Fingerprint, after, bindings, 1000)
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
	skipCode := ""
	for _, id := range job.EmbeddingJobIDs {
		status, statusErr := setup.Service.catalog.EmbeddingJobByID(ctx, id)
		if statusErr != nil {
			return statusErr
		}
		if status.State == "failed" {
			switch status.FailureCode {
			case store.EmbeddingFailureInvalidResponse, store.EmbeddingFailureInputRejected, store.EmbeddingFailureStaleAuthority:
				skipCode = string(status.FailureCode)
			default:
				err = errors.Join(err, fmt.Errorf("local text embedding job %s failed: %s", id, status.FailureCode))
			}
		}
	}
	if err == nil && skipCode != "" {
		setup.skip(item, skipCode)
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
	if len(setup.SelectedProfiles) > 0 {
		for _, lane := range setup.selectedLanes() {
			pending, err := lane.Pending(ctx)
			if err != nil || pending {
				return pending, err
			}
		}
		return false, nil
	}
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
	return setup.backfillProfiles(ctx, logger)
}

// selectedLanes gives every profile its own skip cache and sweep cursor.
func (setup *LocalTextSetup) selectedLanes() []*LocalTextSetup {
	if len(setup.SelectedProfiles) == 0 {
		return []*LocalTextSetup{setup}
	}
	setup.mu.Lock()
	defer setup.mu.Unlock()
	if setup.profileLanes == nil {
		setup.profileLanes = make(map[string]*LocalTextSetup, len(setup.SelectedProfiles))
		for _, name := range setup.SelectedProfiles {
			if name == "" {
				continue
			}
			setup.profileLanes[name] = &LocalTextSetup{Service: setup.Service, Selected: name}
		}
	}
	lanes := make([]*LocalTextSetup, 0, len(setup.profileLanes))
	for _, name := range slices.Sorted(maps.Keys(setup.profileLanes)) {
		lanes = append(lanes, setup.profileLanes[name])
	}
	return lanes
}

type localTextProfileItem struct {
	profile string
	item    store.DocumentSummary
}

func (setup *LocalTextSetup) backfillProfiles(ctx context.Context, logger *slog.Logger) error {
	lanes := setup.selectedLanes()
	byName := make(map[string]*LocalTextSetup, len(lanes))
	for _, lane := range lanes {
		lane.logger = logger
		byName[lane.Selected] = lane
	}
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
	lane := Backfill[localTextProfileItem]{Name: "local-text", Page: 50, IdleDelay: time.Second, Logger: logger,
		WriteRevision: setup.Service.catalog.WriteRevision, Busy: func() bool { return holding },
		List: func(ctx context.Context, after string, limit int) ([]localTextProfileItem, error) {
			profile, cursor, _ := strings.Cut(after, "\x00")
			var result []localTextProfileItem
			for _, current := range lanes {
				if after != "" && current.Selected < profile {
					continue
				}
				start := ""
				if current.Selected == profile {
					start = cursor
				}
				items, err := current.targets(ctx, start, limit-len(result))
				if err != nil {
					return nil, err
				}
				for _, item := range items {
					result = append(result, localTextProfileItem{profile: current.Selected, item: item})
				}
				if len(result) == limit {
					break
				}
			}
			pending := len(result) > 0
			if after == "" && !pending {
				for _, current := range lanes {
					var err error
					pending, err = current.publicationPending(ctx)
					if err != nil {
						return nil, err
					}
					if pending {
						break
					}
				}
			}
			if pending && !holding {
				holding = true
				if setup.ActivityBegin != nil {
					setup.ActivityBegin()
				}
			} else if after == "" && !pending {
				release()
			}
			return result, nil
		},
		Key: func(item localTextProfileItem) string { return item.profile + "\x00" + item.item.ContentVersionID },
		Process: func(ctx context.Context, item localTextProfileItem) error {
			return byName[item.profile].process(ctx, item.item)
		},
	}
	return lane.Run(ctx)
}
