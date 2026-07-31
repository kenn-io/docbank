package blob

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"go.kenn.io/kit/packstore"
	"go.kenn.io/kit/packstore/s3store"
	"go.kenn.io/kit/safefileio"

	"go.kenn.io/docbank/internal/config"
)

// StoreState is the daemon's bounded runtime observation of one cataloged
// physical store. It never rewrites durable authority.
type StoreState string

const (
	storeKindFilesystem = "filesystem"

	StoreOnline        StoreState = "online"
	StoreUnavailable   StoreState = "unavailable"
	StoreFenced        StoreState = "fenced"
	StoreMisconfigured StoreState = "misconfigured"
	StoreUnbound       StoreState = "unbound"
	StoreDetached      StoreState = "detached"
)

var errStoreOwnershipUnavailable = errors.New("store ownership is unavailable")

// StoreSpec is the catalog authority needed to bind one runtime backend.
type StoreSpec struct {
	ID             string
	Kind           string
	Role           string
	Lifecycle      string
	Binding        string
	OwnershipEpoch string
}

// StoreObservation describes one process-local binding result.
type StoreObservation struct {
	State      StoreState
	Detail     string
	ObservedAt time.Time
	Priority   int
}

// Registry owns runtime backends separately from portable catalog authority.
// A missing or bad binding degrades only that store; it never prevents the
// local metadata catalog or other stores from opening.
type Registry struct {
	mu          sync.RWMutex
	vaultID     string
	bindings    map[string]config.StoreBindingConfig
	openBackend configuredBackendFactory
	specs       map[packstore.StoreID]StoreSpec
	backends    map[packstore.StoreID]packstore.Backend
	// Kit's backend registry has no release callback. Once a backend has been
	// handed to a reader, keep a fenced or detached instance alive until daemon
	// shutdown while immediately removing it from admission for new work.
	retired      []packstore.Backend
	observations map[packstore.StoreID]StoreObservation
}

// NewRegistry binds every active secondary that can prove its expected
// ownership marker. The caller supplies catalog rows, never deployment
// endpoints from SQLite.
func NewRegistry(
	ctx context.Context,
	vaultID string,
	bindings map[string]config.StoreBindingConfig,
	stores []StoreSpec,
) *Registry {
	return newRegistry(ctx, vaultID, bindings, stores, newAttachedConfiguredBackend)
}

func newAttachedConfiguredBackend(
	ctx context.Context,
	binding config.StoreBindingConfig,
	expected *packstore.Ownership,
) (packstore.Backend, error) {
	if expected == nil {
		return NewInspectionBackend(ctx, binding)
	}
	if binding.Kind == storeKindFilesystem {
		inspector, err := NewInspectionBackend(ctx, binding)
		if err != nil {
			return nil, err
		}
		actual, ownershipErr := inspector.Ownership(ctx)
		closeErr := closeBackend(inspector)
		if ownershipErr != nil {
			return nil, fmt.Errorf(
				"inspect filesystem store ownership: %w",
				errors.Join(errStoreOwnershipUnavailable, ownershipErr, closeErr),
			)
		}
		if closeErr != nil {
			return nil, closeErr
		}
		if actual != *expected {
			return nil, fmt.Errorf(
				"%w: ownership marker names vault %s store %s epoch %q",
				packstore.ErrStoreFenced, actual.Vault, actual.Store, actual.Epoch,
			)
		}
		if err := EnsureFilesystemNamespace(binding.Path); err != nil {
			return nil, fmt.Errorf("secure filesystem store namespace: %w", err)
		}
	}
	return NewConfiguredBackend(ctx, binding, expected)
}

type configuredBackendFactory func(
	context.Context, config.StoreBindingConfig, *packstore.Ownership,
) (packstore.Backend, error)

func newRegistry(
	ctx context.Context,
	vaultID string,
	bindings map[string]config.StoreBindingConfig,
	stores []StoreSpec,
	openBackend configuredBackendFactory,
) *Registry {
	registry := &Registry{
		vaultID: vaultID, bindings: bindings, openBackend: openBackend,
		specs:        make(map[packstore.StoreID]StoreSpec, len(stores)),
		backends:     make(map[packstore.StoreID]packstore.Backend, len(stores)),
		observations: make(map[packstore.StoreID]StoreObservation, len(stores)),
	}
	for _, spec := range stores {
		id := packstore.StoreID(spec.ID)
		registry.specs[id] = spec
		registry.refreshLocked(ctx, id)
	}
	return registry
}

// Backend implements Kit's read-backend registry.
func (r *Registry) Backend(id packstore.StoreID) (packstore.ReadBackend, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	backend, ok := r.backends[id]
	return backend, ok
}

// WritableBackend returns one fully fenced backend for explicit placement.
func (r *Registry) WritableBackend(id packstore.StoreID) (packstore.Backend, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	backend, ok := r.backends[id]
	return backend, ok
}

// Observation returns the last bounded runtime state.
func (r *Registry) Observation(id string) StoreObservation {
	r.mu.RLock()
	defer r.mu.RUnlock()
	observation, ok := r.observations[packstore.StoreID(id)]
	if !ok {
		return StoreObservation{State: StoreUnbound, Detail: "store is not cataloged"}
	}
	return observation
}

// Refresh performs a fresh marker check for one cataloged store.
func (r *Registry) Refresh(ctx context.Context, id string) StoreObservation {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.refreshLocked(ctx, packstore.StoreID(id))
	return r.observations[packstore.StoreID(id)]
}

// SalvageBackend opens a fenced namespace for explicit verified read-only
// recovery. It never admits publication, retirement, or ordinary reads.
func (r *Registry) SalvageBackend(
	ctx context.Context, id packstore.StoreID,
) (packstore.Backend, error) {
	r.mu.RLock()
	spec, known := r.specs[id]
	observation := r.observations[id]
	binding, bound := r.bindings[spec.Binding]
	r.mu.RUnlock()
	if !known {
		return nil, fmt.Errorf("salvage store %s is not cataloged", id)
	}
	if observation.State != StoreFenced {
		return nil, fmt.Errorf(
			"salvage store %s is %s, not fenced", id, observation.State,
		)
	}
	if !bound || binding.Kind != spec.Kind {
		return nil, fmt.Errorf("salvage store %s has no usable binding", id)
	}
	return r.openBackend(ctx, binding, nil)
}

// AttachSpec makes a newly committed catalog store available to this daemon
// without reloading deployment configuration.
func (r *Registry) AttachSpec(ctx context.Context, spec StoreSpec) StoreObservation {
	r.mu.Lock()
	defer r.mu.Unlock()
	id := packstore.StoreID(spec.ID)
	r.specs[id] = spec
	r.refreshLocked(ctx, id)
	return r.observations[id]
}

// RemoveSpec drops one unregistered store from runtime admission.
func (r *Registry) RemoveSpec(id string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	key := packstore.StoreID(id)
	r.retireBackendLocked(key)
	delete(r.specs, key)
	delete(r.observations, key)
}

// Binding returns one daemon-loaded deployment profile.
func (r *Registry) Binding(name string) (config.StoreBindingConfig, bool) {
	binding, ok := r.bindings[name]
	return binding, ok
}

func (r *Registry) refreshLocked(ctx context.Context, id packstore.StoreID) {
	spec, ok := r.specs[id]
	if !ok {
		r.retireBackendLocked(id)
		r.observe(id, StoreUnbound, "store is not cataloged", 0)
		return
	}
	if spec.Lifecycle == "detached" {
		r.retireBackendLocked(id)
		r.observe(id, StoreDetached, "store is detached", 0)
		return
	}
	binding, ok := r.bindings[spec.Binding]
	if !ok {
		r.retireBackendLocked(id)
		r.observe(id, StoreUnbound,
			fmt.Sprintf("binding profile %q is not loaded; restart after updating config.toml", spec.Binding),
			0)
		return
	}
	if binding.Kind != spec.Kind {
		r.retireBackendLocked(id)
		r.observe(id, StoreMisconfigured,
			fmt.Sprintf("binding %q has kind %q, catalog expects %q",
				spec.Binding, binding.Kind, spec.Kind),
			binding.Priority)
		return
	}
	expected := packstore.Ownership{
		Format: packstore.OwnershipFormatV1, Vault: r.vaultID,
		Store: id, Epoch: spec.OwnershipEpoch,
	}
	if prior := r.backends[id]; prior != nil {
		actual, err := prior.Ownership(ctx)
		if err == nil && actual == expected {
			r.observe(id, StoreOnline, "", binding.Priority)
			return
		}
		r.retireBackendLocked(id)
		if err != nil {
			var mismatch *packstore.OwnershipMismatchError
			if errors.As(err, &mismatch) || errors.Is(err, packstore.ErrStoreFenced) {
				r.observe(id, StoreFenced, err.Error(), binding.Priority)
			} else {
				r.observe(id, StoreUnavailable, err.Error(), binding.Priority)
			}
			return
		}
		r.observe(id, StoreFenced,
			fmt.Sprintf("ownership marker names store %s epoch %q", actual.Store, actual.Epoch),
			binding.Priority)
		return
	}
	backend, err := r.openBackend(ctx, binding, &expected)
	if err != nil {
		if errors.Is(err, packstore.ErrStoreFenced) {
			r.observe(id, StoreFenced, err.Error(), binding.Priority)
		} else if errors.Is(err, errStoreOwnershipUnavailable) {
			r.observe(id, StoreUnavailable, err.Error(), binding.Priority)
		} else {
			r.observe(id, StoreMisconfigured, err.Error(), binding.Priority)
		}
		return
	}
	actual, err := backend.Ownership(ctx)
	if err != nil {
		var mismatch *packstore.OwnershipMismatchError
		if errors.As(err, &mismatch) || errors.Is(err, packstore.ErrStoreFenced) {
			r.observe(id, StoreFenced, err.Error(), binding.Priority)
		} else {
			r.observe(id, StoreUnavailable, err.Error(), binding.Priority)
		}
		_ = closeBackend(backend)
		return
	}
	if actual != expected {
		r.observe(id, StoreFenced,
			fmt.Sprintf("ownership marker names store %s epoch %q", actual.Store, actual.Epoch),
			binding.Priority)
		_ = closeBackend(backend)
		return
	}
	r.backends[id] = backend
	r.observe(id, StoreOnline, "", binding.Priority)
}

func (r *Registry) retireBackendLocked(id packstore.StoreID) {
	if backend := r.backends[id]; backend != nil {
		r.retired = append(r.retired, backend)
		delete(r.backends, id)
	}
}

func (r *Registry) observe(id packstore.StoreID, state StoreState, detail string, priority int) {
	r.observations[id] = StoreObservation{
		State: state, Detail: detail, Priority: priority, ObservedAt: time.Now().UTC(),
	}
}

// Close releases backend pack readers.
func (r *Registry) Close() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	var result error
	for id, backend := range r.backends {
		result = errors.Join(result, closeBackend(backend))
		delete(r.backends, id)
	}
	for _, backend := range r.retired {
		result = errors.Join(result, closeBackend(backend))
	}
	r.retired = nil
	return result
}

func closeBackend(backend packstore.Backend) error {
	if closer, ok := backend.(io.Closer); ok {
		return closer.Close()
	}
	return nil
}

// NewFilesystemBackend constructs an attached or unattached backend over one
// deployment binding. Registration uses an unattached backend to inspect and
// replace the marker before catalog commit.
func NewFilesystemBackend(
	path string, expected *packstore.Ownership,
) (*packstore.FilesystemBackend, error) {
	return newFilesystemBackend(path, expected, true)
}

func newFilesystemBackend(
	path string, expected *packstore.Ownership, validate bool,
) (*packstore.FilesystemBackend, error) {
	layout, err := packstore.NewLayout(path, packstore.LayoutOptions{
		Staging: packstore.StagingStoreDirectory, StagingDir: "tmp",
	})
	if err != nil {
		return nil, fmt.Errorf("create filesystem blob layout: %w", err)
	}
	if validate {
		if err := validateFilesystemNamespace(layout.Root()); err != nil {
			return nil, fmt.Errorf("validate private filesystem blob layout: %w", err)
		}
	}
	backend, err := packstore.NewFilesystemBackend(layout, packstore.FilesystemBackendOptions{
		ExpectedOwnership: expected,
		Limits:            StorageLimits(),
	})
	if err != nil {
		return nil, fmt.Errorf("create filesystem blob backend: %w", err)
	}
	return backend, nil
}

// NewInspectionBackend opens a configured namespace without repairing it or
// granting catalog authority. Admission previews and restore preflight use it
// only to inspect ownership and emptiness before any local mutation.
func NewInspectionBackend(
	ctx context.Context, binding config.StoreBindingConfig,
) (packstore.Backend, error) {
	if binding.Kind == storeKindFilesystem {
		return newFilesystemBackend(binding.Path, nil, false)
	}
	return NewConfiguredBackend(ctx, binding, nil)
}

// EnsureFilesystemNamespace securely creates or repairs one filesystem store
// before registration or restore changes its ownership marker.
func EnsureFilesystemNamespace(path string) error {
	layout, err := packstore.NewLayout(path, packstore.LayoutOptions{
		Staging: packstore.StagingStoreDirectory, StagingDir: "tmp",
	})
	if err != nil {
		return fmt.Errorf("create filesystem blob layout: %w", err)
	}
	root := layout.Root()
	if err := safefileio.EnsurePrivateDir(root); err != nil {
		return fmt.Errorf("secure filesystem store root: %w", err)
	}
	return checkFilesystemScaffolding(root, safefileio.EnsurePrivateDir)
}

func validateFilesystemNamespace(root string) error {
	if _, err := os.Lstat(root); err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	if err := safefileio.ValidatePrivateDir(root); err != nil {
		return fmt.Errorf("validate filesystem store root: %w", err)
	}
	return checkFilesystemScaffolding(root, safefileio.ValidatePrivateDir)
}

func checkFilesystemScaffolding(root string, check func(string) error) error {
	secureChildren := func(parent string) error {
		entries, err := os.ReadDir(parent)
		if err != nil {
			return err
		}
		for _, entry := range entries {
			path := filepath.Join(parent, entry.Name())
			if entry.Type()&os.ModeSymlink != 0 {
				return fmt.Errorf("filesystem store scaffolding %s is a symlink", path)
			}
			if entry.IsDir() {
				if err := check(path); err != nil {
					return err
				}
			}
		}
		return nil
	}
	if err := secureChildren(root); err != nil {
		return err
	}
	packs := filepath.Join(root, "packs")
	if info, err := os.Lstat(packs); err == nil && info.IsDir() {
		if err := secureChildren(packs); err != nil {
			return err
		}
	} else if err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

// NewConfiguredBackend constructs one deployment backend without granting
// catalog authority. Expected ownership enables destructive work; nil is
// reserved for registration and explicit fenced-store salvage.
func NewConfiguredBackend(
	ctx context.Context,
	binding config.StoreBindingConfig,
	expected *packstore.Ownership,
) (packstore.Backend, error) {
	return newConfiguredBackend(ctx, binding, expected, false)
}

func newConfiguredBackend(
	ctx context.Context,
	binding config.StoreBindingConfig,
	expected *packstore.Ownership,
	allowInsecureTransport bool,
) (packstore.Backend, error) {
	switch binding.Kind {
	case storeKindFilesystem:
		return NewFilesystemBackend(binding.Path, expected)
	case "s3":
		if err := validateS3Transport(binding.Endpoint, allowInsecureTransport); err != nil {
			return nil, err
		}
		loadOptions := []func(*awsconfig.LoadOptions) error{}
		if binding.Region != "" {
			loadOptions = append(loadOptions, awsconfig.WithRegion(binding.Region))
		}
		if binding.CredentialProfile != "" {
			loadOptions = append(
				loadOptions,
				awsconfig.WithSharedConfigProfile(binding.CredentialProfile),
			)
		}
		loaded, err := awsconfig.LoadDefaultConfig(ctx, loadOptions...)
		if err != nil {
			return nil, fmt.Errorf(
				"load S3 credential profile %q: %w",
				binding.CredentialProfile, err,
			)
		}
		backend, err := s3store.New(ctx, s3store.Config{
			Endpoint: binding.Endpoint, Region: binding.Region,
			Bucket: binding.Bucket, Prefix: binding.Prefix,
			Credentials: loaded.Credentials, ForcePathStyle: binding.ForcePathStyle,
			AllowInsecureTransport: allowInsecureTransport,
			ExpectedOwnership:      expected, Limits: StorageLimits(),
		})
		if err != nil {
			return nil, fmt.Errorf("create S3 blob backend: %w", err)
		}
		return backend, nil
	default:
		return nil, fmt.Errorf("unsupported backend kind %q", binding.Kind)
	}
}

func validateS3Transport(raw string, allowInsecure bool) error {
	if raw == "" {
		return nil
	}
	endpoint, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("parse S3 endpoint: %w", err)
	}
	if strings.EqualFold(endpoint.Scheme, "https") ||
		(allowInsecure && strings.EqualFold(endpoint.Scheme, "http")) {
		return nil
	}
	return errors.New("S3 endpoint must use authenticated HTTPS transport")
}

// ProbeConfiguredBackend validates behavior that cannot be inferred from
// static configuration. Filesystem semantics are exercised by construction;
// S3-compatible services must pass Kit's live capability probe.
func ProbeConfiguredBackend(
	ctx context.Context, backend packstore.Backend,
) error {
	if remote, ok := backend.(*s3store.Backend); ok {
		if _, err := remote.Probe(ctx); err != nil {
			return fmt.Errorf("probe S3-compatible backend: %w", err)
		}
	}
	return nil
}

type orderedRegistry struct {
	primaryID   packstore.StoreID
	primary     packstore.ReadBackend
	secondaries *Registry
}

func (r orderedRegistry) Backend(id packstore.StoreID) (packstore.ReadBackend, bool) {
	if id == r.primaryID {
		return r.primary, true
	}
	if r.secondaries == nil {
		return nil, false
	}
	return r.secondaries.Backend(id)
}

type orderedResolver struct {
	resolver  packstore.LocationResolver
	primaryID packstore.StoreID
	registry  *Registry
}

func (r orderedResolver) ResolveLocations(
	ctx context.Context, hash packstore.Hash,
) (packstore.Resolution, error) {
	resolution, err := r.resolver.ResolveLocations(ctx, hash)
	if err != nil {
		return resolution, fmt.Errorf("resolve blob locations: %w", err)
	}
	if len(resolution.Candidates) < 2 {
		return resolution, nil
	}
	sort.SliceStable(resolution.Candidates, func(i, j int) bool {
		return r.rank(resolution.Candidates[i]) < r.rank(resolution.Candidates[j])
	})
	return resolution, nil
}

func (r orderedResolver) rank(location packstore.ReadLocation) int64 {
	if location.StoreID == r.primaryID {
		return 0
	}
	observation := r.registry.Observation(string(location.StoreID))
	priority := max(observation.Priority, 1)
	return int64(priority)<<32 | int64(stableStoreRank(location.StoreID))
}

func stableStoreRank(id packstore.StoreID) uint32 {
	var rank uint32
	for _, value := range []byte(id) {
		rank = rank*33 + uint32(value)
	}
	return rank
}
