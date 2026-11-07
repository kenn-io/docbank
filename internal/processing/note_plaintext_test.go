package processing

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/document/plaintext"
	"go.kenn.io/docbank/internal/blob"
	"go.kenn.io/docbank/internal/store"
	"hegel.dev/go/hegel"
)

type notePlaintextFixture struct {
	catalog *store.Store
	blobs   *blob.Store
	service *Service
	worker  *RenditionWorker
}

func newNotePlaintextFixture(t testing.TB) notePlaintextFixture {
	t.Helper()
	root := t.TempDir()
	catalog, err := store.Open(t.Context(), filepath.Join(root, "catalog.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, catalog.Close()) })
	blobs, err := blob.New(store.NewPackCatalog(catalog), filepath.Join(root, "blobs"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, blobs.Close()) })
	name, profile, err := NewNotePlaintextProfile()
	require.NoError(t, err)
	gate := newWorkerTestGate()
	require.NoError(t, os.Mkdir(filepath.Join(root, "spool"), 0o700))
	s, err := NewService(t.Context(), ServiceConfig{Catalog: catalog, Blobs: blobs, Gate: gate,
		Profiles: map[string]ProfileConfig{name: profile}, Principal: "synthetic:publisher", Scope: "synthetic-note-scope",
		SpoolDirectory: filepath.Join(root, "spool")})
	require.NoError(t, err)
	t.Cleanup(s.Stop)
	worker, err := NewRenditionWorker(RenditionWorkerConfig{Catalog: catalog, Blobs: blobs,
		Runtime: s.renditions, Gate: gate, Owner: "synthetic-note-worker", LeaseDuration: time.Minute, IdleDelay: time.Millisecond})
	require.NoError(t, err)
	return notePlaintextFixture{catalog, blobs, s, worker}
}

func (f notePlaintextFixture) note(t testing.TB, name, text string) store.Node {
	t.Helper()
	receipt, err := f.blobs.WriteDetailedContext(t.Context(), bytes.NewBufferString(text))
	require.NoError(t, err)
	node, err := f.catalog.CreateFile(t.Context(), f.catalog.RootID(), name, receipt.Hash, receipt.Size, "text/markdown", processingBlobPhysical(t, receipt))
	require.NoError(t, err)
	return node
}

func TestNotePlaintextPlanIsFixedLocalOnly(t *testing.T) {
	t.Parallel()
	name, config, err := NewNotePlaintextProfile()
	require.NoError(t, err)
	require.Equal(t, "brain-local-text", name)
	require.Equal(t, name, NotePlaintextProfileName)
	require.IsType(t, (*plaintext.Provider)(nil), config.RenditionProvider)
	require.Empty(t, config.Profile.Embeddings)
	require.Empty(t, config.EmbeddingProviders)
	require.Empty(t, config.Tokenizers)
	require.Nil(t, config.RerankingProvider)
	canonical, fingerprints, err := document.CanonicalProfile(config.Profile)
	require.NoError(t, err)
	_, again, err := NewNotePlaintextProfile()
	require.NoError(t, err)
	other, otherFP, err := document.CanonicalProfile(again.Profile)
	require.NoError(t, err)
	require.Equal(t, canonical, other)
	require.Equal(t, fingerprints, otherFP)
	// Constructors return independent mutable configs, not a shared profile.
	again.Profile.Rendition.DiscloseFilename = true
	require.False(t, config.Profile.Rendition.DiscloseFilename)
	f := newNotePlaintextFixture(t)
	plan, request, err := f.service.NoteLocalTextPlan(t.Context())
	require.NoError(t, err)
	require.Equal(t, Selector{Profile: name}, plan.Selector)
	require.Equal(t, f.catalog.VaultID(), plan.VaultUID)
	require.Equal(t, fingerprints.Profile, plan.ProfileFingerprint)
	require.Equal(t, "required", plan.ConsentState)
	require.True(t, plan.ConsentRequired)
	require.Len(t, plan.Flow, 1)
	hop := plan.Flow[0]
	require.Equal(t, "rendition", hop.Capability)
	require.Equal(t, "plaintext.in-process-v1", hop.ProviderID)
	require.Equal(t, string(document.RenditionTrustLocalProcess), hop.TrustBoundary)
	require.Equal(t, "in-process", hop.RuntimeDisclosure.Endpoint)
	require.Empty(t, hop.RuntimeDisclosure.Model)
	require.False(t, hop.DiscloseFilename)
	require.Empty(t, hop.Filename)
	require.Equal(t, []string{"original_file"}, plan.DisclosedClasses)
	require.Equal(t, []string{"normalized_evidence", "sanitized_markdown"}, plan.RetainedClasses)
	require.Equal(t, Estimate{ProviderCalls: 1}, plan.Estimate)
	require.Equal(t, document.ProcessingConsentRequest{Principal: "synthetic:publisher", Scope: "synthetic-note-scope",
		ProfileFingerprint: fingerprints.Profile, DisclosureFingerprint: config.Profile.Rendition.DisclosureFingerprint,
		InputClasses: []string{"original_file"}, RetainedArtifactClasses: []string{"normalized_evidence", "sanitized_markdown"}}, request)
	_, err = f.catalog.AuthorizeProviderOperation(t.Context(), f.service.renditionConsentRequest(f.service.profiles[name]))
	require.ErrorIs(t, err, store.ErrProcessingConsentRequired)
	processed, err := f.worker.RunOne(t.Context())
	require.NoError(t, err)
	require.False(t, processed)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, _, err = f.service.NoteLocalTextPlan(ctx)
	require.ErrorIs(t, err, context.Canceled)
	delete(f.service.profiles, name)
	_, _, err = f.service.NoteLocalTextPlan(t.Context())
	require.ErrorIs(t, err, ErrProfileNotConfigured)
}

func TestNotePlaintextRequiresLiveConsent(t *testing.T) {
	t.Parallel()
	f := newNotePlaintextFixture(t)
	node := f.note(t, "entry.md", "# Heading\nSilvermoon body evidence\n")
	plan, request, err := f.service.NoteLocalTextPlan(t.Context())
	require.NoError(t, err)
	require.NoError(t, f.service.AdmitImportedRendition(t.Context(), node, NotePlaintextProfileName))
	processed, err := f.worker.RunOne(t.Context())
	require.NoError(t, err)
	require.False(t, processed)
	// This is the same public store boundary used by ordinary scoped consent.
	_, err = f.catalog.GrantProcessingConsent(t.Context(), request)
	require.NoError(t, err)
	active, _, err := f.service.NoteLocalTextPlan(t.Context())
	require.NoError(t, err)
	require.Equal(t, "active", active.ConsentState)
	require.False(t, active.ConsentRequired)
	require.Equal(t, plan.Fingerprint, active.Fingerprint)
	require.NoError(t, f.service.AdmitImportedRendition(t.Context(), node, NotePlaintextProfileName))
	processed, err = f.worker.RunOne(t.Context())
	require.NoError(t, err)
	require.True(t, processed)
	hits, _, err := f.catalog.SearchPage(t.Context(), "Silvermoon", 20)
	require.NoError(t, err)
	require.Len(t, hits, 1)
	require.Equal(t, node.CurrentVersionID, hits[0].Node.CurrentVersionID)
	require.Empty(t, f.service.profiles[NotePlaintextProfileName].embedders)
	require.Empty(t, f.service.profiles[NotePlaintextProfileName].embeddingRuntimes)
}

func TestNotePlaintextRevocation(t *testing.T) {
	t.Parallel()
	f := newNotePlaintextFixture(t)
	node := f.note(t, "queued.md", "Revocationword remains unindexed\n")
	original, request, err := f.service.NoteLocalTextPlan(t.Context())
	require.NoError(t, err)
	_, err = f.catalog.GrantProcessingConsent(t.Context(), request)
	require.NoError(t, err)
	require.NoError(t, f.service.AdmitImportedRendition(t.Context(), node, NotePlaintextProfileName))
	_, err = f.service.RevokeConsent(t.Context())
	require.NoError(t, err)
	revoked, _, err := f.service.NoteLocalTextPlan(t.Context())
	require.NoError(t, err)
	require.Equal(t, "revoked", revoked.ConsentState)
	require.True(t, revoked.ConsentRequired)
	require.Equal(t, original.Fingerprint, revoked.Fingerprint)
	_, err = f.worker.RunOne(t.Context())
	require.NoError(t, err)
	hits, _, err := f.catalog.SearchPage(t.Context(), "Revocationword", 20)
	require.NoError(t, err)
	require.Empty(t, hits)
	// Recovery admission must also recheck the live grant.
	require.NoError(t, f.service.AdmitImportedRendition(t.Context(), node, NotePlaintextProfileName))
	processed, err := f.worker.RunOne(t.Context())
	require.NoError(t, err)
	require.False(t, processed)
	request.ExpiresAt = new(time.Now().Add(-time.Minute))
	_, err = f.catalog.GrantProcessingConsent(t.Context(), request)
	require.NoError(t, err)
	expired, _, err := f.service.NoteLocalTextPlan(t.Context())
	require.NoError(t, err)
	require.Equal(t, "expired", expired.ConsentState)
	require.True(t, expired.ConsentRequired)
	require.Equal(t, original.Fingerprint, expired.Fingerprint)
}

// Each mutation would turn a fixed local-only review into different processing
// or consent authority. All must fail before a plan or grant request is exposed.
func notePlaintextMutations() map[string]func(*configuredProfile) {
	return map[string]func(*configuredProfile){
		"filename":          func(p *configuredProfile) { p.portable.Rendition.DiscloseFilename = true },
		"disclosure":        func(p *configuredProfile) { p.portable.Rendition.DisclosureFingerprint = strings.Repeat("a", 64) },
		"credential":        func(p *configuredProfile) { p.portable.Rendition.CredentialBinding = "credential:other" },
		"retention":         func(p *configuredProfile) { p.portable.RetentionDisclosure.RetainProviderMarkdown = true },
		"embedding":         func(p *configuredProfile) { p.portable.Embeddings = []document.EmbeddingBindingV1{{Name: "other"}} },
		"limit":             func(p *configuredProfile) { p.portable.Rendition.MaxDocumentBytes-- },
		"profile record":    func(p *configuredProfile) { p.record.Fingerprint = strings.Repeat("b", 64) },
		"consent record":    func(p *configuredProfile) { p.record.RenditionDisclosureFingerprint = strings.Repeat("c", 64) },
		"network endpoint":  func(p *configuredProfile) { p.renderDisclosure.Endpoint = "https://provider.invalid" },
		"processor":         func(p *configuredProfile) { p.renderDisclosure.UltimateProcessor = "other-processor" },
		"model":             func(p *configuredProfile) { p.renderDisclosure.Model = "other-model" },
		"metadata":          func(p *configuredProfile) { p.renderDisclosure.MetadataClasses = []string{"filename"} },
		"runtime retention": func(p *configuredProfile) { p.renderDisclosure.RetainedArtifactRoles = []string{"original_file"} },
		"wrapped provider":  func(p *configuredProfile) { p.provider = struct{ document.RenditionProvider }{p.provider} },
		"missing provider":  func(p *configuredProfile) { p.provider = nil },
		"rerank disclosure": func(p *configuredProfile) { p.rerankingDisclosure.Endpoint = "https://rerank.invalid" },
		"embedding runtime": func(p *configuredProfile) { p.embeddingRuntimes = map[string]*ProviderEmbeddingRuntime{"other": {}} },
		"vector space":      func(p *configuredProfile) { p.vectorSpaces = map[string]string{"other": "other-vector-space"} },
	}
}

func TestNotePlaintextRejectsChangedProfile(t *testing.T) {
	for name, mutate := range notePlaintextMutations() {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			f := newNotePlaintextFixture(t)
			profile := f.service.profiles[NotePlaintextProfileName]
			mutate(&profile)
			f.service.profiles[NotePlaintextProfileName] = profile
			plan, request, err := f.service.NoteLocalTextPlan(t.Context())
			require.ErrorIs(t, err, ErrPlanChanged)
			require.Zero(t, plan)
			require.Zero(t, request)
		})
	}
}

func TestNotePlaintextConsentScopeAndOtherConnection(t *testing.T) {
	t.Parallel()
	f := newNotePlaintextFixture(t)
	_, request, err := f.service.NoteLocalTextPlan(t.Context())
	require.NoError(t, err)
	wrongScope := request
	wrongScope.Scope = "other-note-scope"
	_, err = f.catalog.GrantProcessingConsent(t.Context(), wrongScope)
	require.NoError(t, err)
	wrongPrincipal := request
	wrongPrincipal.Principal = "synthetic:other-publisher"
	_, err = f.catalog.GrantProcessingConsent(t.Context(), wrongPrincipal)
	require.NoError(t, err)
	plan, _, err := f.service.NoteLocalTextPlan(t.Context())
	require.NoError(t, err)
	require.Equal(t, "required", plan.ConsentState)
	// A separate service using the same ordinary authority observes the grant.
	name, profile, err := NewNotePlaintextProfile()
	require.NoError(t, err)
	other, err := NewService(t.Context(), ServiceConfig{Catalog: f.catalog, Blobs: f.blobs, Gate: newWorkerTestGate(),
		Profiles: map[string]ProfileConfig{name: profile}, Principal: request.Principal, Scope: request.Scope, SpoolDirectory: t.TempDir()})
	require.NoError(t, err)
	t.Cleanup(other.Stop)
	_, otherRequest, err := other.NoteLocalTextPlan(t.Context())
	require.NoError(t, err)
	_, err = f.catalog.GrantProcessingConsent(t.Context(), otherRequest)
	require.NoError(t, err)
	active, _, err := f.service.NoteLocalTextPlan(t.Context())
	require.NoError(t, err)
	require.Equal(t, "active", active.ConsentState)
	require.False(t, active.ConsentRequired)
	require.Equal(t, plan.Fingerprint, active.Fingerprint)
}

func TestNotePlaintextReadinessBindsExactVersion(t *testing.T) {
	t.Parallel()
	f := newNotePlaintextFixture(t)
	node := f.note(t, "versioned.md", "Firstversionword source\n")
	_, request, err := f.service.NoteLocalTextPlan(t.Context())
	require.NoError(t, err)
	_, err = f.catalog.GrantProcessingConsent(t.Context(), request)
	require.NoError(t, err)
	require.NoError(t, f.service.AdmitImportedRendition(t.Context(), node, NotePlaintextProfileName))
	processed, err := f.worker.RunOne(t.Context())
	require.NoError(t, err)
	require.True(t, processed)
	coverage, err := f.service.Coverage(t.Context(), NotePlaintextProfileName,
		SourceFence{VaultUID: f.catalog.VaultID(), ContentVersionIDs: []string{node.CurrentVersionID}})
	require.NoError(t, err)
	require.Equal(t, 1, coverage.Renditions.Complete)
	receipt, err := f.blobs.WriteDetailedContext(t.Context(), strings.NewReader("Secondversionword source\n"))
	require.NoError(t, err)
	changed, _, err := f.catalog.ReplaceContent(t.Context(), node.ID, node.Revision, receipt.Hash, receipt.Size,
		"text/markdown", processingBlobPhysical(t, receipt))
	require.NoError(t, err)
	require.NotEqual(t, node.CurrentVersionID, changed.CurrentVersionID)
	coverage, err = f.service.Coverage(t.Context(), NotePlaintextProfileName,
		SourceFence{VaultUID: f.catalog.VaultID(), ContentVersionIDs: []string{changed.CurrentVersionID}})
	require.NoError(t, err)
	require.Equal(t, 1, coverage.Renditions.Total)
	require.Zero(t, coverage.Renditions.Complete)
	require.Empty(t, coverage.Embeddings)
	hits, _, err := f.catalog.SearchPage(t.Context(), "Secondversionword", 20)
	require.NoError(t, err)
	require.Empty(t, hits)
	require.NoError(t, f.service.AdmitImportedRendition(t.Context(), changed, NotePlaintextProfileName))
	processed, err = f.worker.RunOne(t.Context())
	require.NoError(t, err)
	require.True(t, processed)
	hits, _, err = f.catalog.SearchPage(t.Context(), "Secondversionword", 20)
	require.NoError(t, err)
	require.Len(t, hits, 1)
	require.Equal(t, changed.CurrentVersionID, hits[0].Node.CurrentVersionID)
}

// Combining changed policy, execution, and disclosure must never yield an
// approvable fixed local plan, even if the saved record still names the builtin.
func TestNotePlaintextFixedPlanProperty(t *testing.T) {
	if runtime.GOOS == "darwin" && runtime.GOARCH != "arm64" {
		t.Skip("Hegel native engine requires macOS arm64")
	}
	f := newNotePlaintextFixture(t)
	base := f.service.profiles[NotePlaintextProfileName]
	mutations := notePlaintextMutations()
	names := make([]string, 0, len(mutations))
	for name := range mutations {
		names = append(names, name)
	}
	sort.Strings(names)
	hegel.Test(t, func(ht *hegel.T) {
		profile := base
		var err error
		profile.portable, err = document.CanonicalizeProfile(base.portable)
		require.NoError(ht, err)
		changed := hegel.Draw(ht, hegel.Booleans())
		if changed {
			mask := hegel.Draw(ht, hegel.Integers(1, (1<<len(names))-1))
			for i, name := range names {
				if mask&(1<<i) != 0 {
					mutations[name](&profile)
				}
			}
		}
		f.service.profiles[NotePlaintextProfileName] = profile
		plan, request, err := f.service.NoteLocalTextPlan(t.Context())
		if changed {
			require.ErrorIs(ht, err, ErrPlanChanged)
			require.Zero(ht, plan)
			require.Zero(ht, request)
			return
		}
		require.NoError(ht, err)
		require.Equal(ht, "required", plan.ConsentState)
		require.True(ht, plan.ConsentRequired)
		require.Len(ht, plan.Flow, 1)
		require.Equal(ht, "in-process", plan.Flow[0].RuntimeDisclosure.Endpoint)
		require.Equal(ht, []string{"original_file"}, request.InputClasses)
		require.Equal(ht, []string{"normalized_evidence", "sanitized_markdown"}, request.RetainedArtifactClasses)
	}, hegel.WithDatabase(t.TempDir()), hegel.WithTestCases(2000))
}

// The oracle follows ordinary scoped events, independently of the service's
// authorization/error mapping. Plans must observe each event without caching it
// or changing the review fingerprint, and unrelated grants have no effect.
func TestNotePlaintextLiveConsentProperty(t *testing.T) {
	if runtime.GOOS == "darwin" && runtime.GOARCH != "arm64" {
		t.Skip("Hegel native engine requires macOS arm64")
	}
	f := newNotePlaintextFixture(t)
	caseID := 0
	hegel.Test(t, func(ht *hegel.T) {
		caseID++
		f.service.principal = fmt.Sprintf("synthetic:generated-%d", caseID)
		initial, request, err := f.service.NoteLocalTextPlan(t.Context())
		require.NoError(ht, err)
		want := "required"
		require.Equal(ht, want, initial.ConsentState)
		for range hegel.Draw(ht, hegel.Integers(1, 6)) {
			switch hegel.Draw(ht, hegel.Integers(0, 4)) {
			case 0:
				_, err = f.catalog.GrantProcessingConsent(t.Context(), request)
				want = "active"
			case 1:
				_, err = f.service.RevokeConsent(t.Context())
				// Revocation fences grants; an authority that never held a
				// matching grant still requires its first grant.
				if want != "required" {
					want = "revoked"
				}
			case 2:
				expired := request
				expired.ExpiresAt = new(time.Now().Add(-time.Hour))
				_, err = f.catalog.GrantProcessingConsent(t.Context(), expired)
				// An earlier live grant can still authorize the exact operation.
				if want != "active" {
					want = "expired"
				}
			case 3:
				other := request
				other.Scope += "-other"
				_, err = f.catalog.GrantProcessingConsent(t.Context(), other)
			case 4:
				other := request
				other.DisclosureFingerprint = strings.Repeat("d", 64)
				_, err = f.catalog.GrantProcessingConsent(t.Context(), other)
			}
			require.NoError(ht, err)
			plan, gotRequest, err := f.service.NoteLocalTextPlan(t.Context())
			require.NoError(ht, err)
			require.Equal(ht, want, plan.ConsentState)
			require.Equal(ht, want != "active", plan.ConsentRequired)
			require.Equal(ht, initial.Fingerprint, plan.Fingerprint)
			require.Equal(ht, request, gotRequest)
		}
	}, hegel.WithDatabase(t.TempDir()), hegel.WithTestCases(2000))
}
