package processing

import (
	"context"
	"encoding/json/v2"
	"sync"

	"go.kenn.io/docbank/internal/config"
	"go.kenn.io/docbank/internal/store"
)

// FaceSetup reviews and grants the existing processing consent for local faces.
// Its runtime callback only reuses installed assets.
type FaceSetup struct {
	Service *Service
	Startup func(context.Context) error
	mu      sync.Mutex
}

func (setup *FaceSetup) Plan(ctx context.Context) (LocalTextPlan, error) {
	profile, ok := setup.Service.profiles[config.LocalFacesProfile]
	if !ok {
		return LocalTextPlan{}, ErrProfileNotConfigured
	}
	operation, err := setup.Service.planForSource(ctx, Selector{Profile: config.LocalFacesProfile}, store.Node{}, store.ContentVersion{}, profile)
	if err != nil {
		return LocalTextPlan{}, err
	}
	plan := LocalTextPlan{VaultUID: setup.Service.catalog.VaultID(), Profile: config.LocalFacesProfile, ProfileFingerprint: profile.record.Fingerprint, Managed: true, Models: []string{profile.portable.Embeddings[0].Model}, Flow: operation.Flow, RetainedClasses: operation.RetainedClasses, Scope: photoDiscoveryScope(profile), VectorEffect: "Adds local face suggestions", Server: "local"}
	data, err := json.Marshal(plan, json.Deterministic(true))
	if err != nil {
		return plan, err
	}
	plan.Fingerprint = workerHashBytes(data)
	return plan, nil
}
func (setup *FaceSetup) Apply(ctx context.Context, fingerprint string) (LocalTextPlan, error) {
	setup.mu.Lock()
	defer setup.mu.Unlock()
	plan, err := setup.Plan(ctx)
	if err != nil {
		return plan, err
	}
	if fingerprint == "" || fingerprint != plan.Fingerprint {
		return plan, ErrPlanChanged
	}
	if err := setup.Startup(ctx); err != nil {
		return plan, err
	}
	return plan, setup.Service.grantProfileConsent(ctx, setup.Service.profiles[config.LocalFacesProfile], nil)
}
