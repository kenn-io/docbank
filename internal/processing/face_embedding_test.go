package processing

import (
	"bytes"
	"context"
	"encoding/json/v2"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"slices"
	"testing"
	"time"
	"uuid"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/document/localface"
	"go.kenn.io/docbank/internal/config"
	"go.kenn.io/docbank/internal/store"
	"go.kenn.io/kit/embedclient"
	"go.kenn.io/kit/packstore"
)

type faceEmbeddingFixture struct {
	f            *photoDescriptionFixture
	service      *Service
	worker       *EmbeddingWorker
	calls        int
	boxes        []embedclient.FaceBox
	inputs       []store.PhotoFace
	unmatched    bool
	matchAll     bool
	vectorForBox func(embedclient.FaceBox) []float32
}

func newFaceEmbeddingFixture(t *testing.T, unmatched bool) *faceEmbeddingFixture {
	t.Helper()
	ctx := t.Context()
	f := newPhotoDescriptionFixture(t)
	detector := f.newVisionServiceWith(t, map[string]string{"faces": "detector"}, document.FaceDetectionProviderID)
	f.reply = detectedFaceReply
	f.enqueueFaceDetection(t, detector)
	f.runWorker(t, detector)
	_, _, err := f.catalog.CreatePhotoFace(ctx, f.node.CurrentVersionID, uuid.New().String(), document.FaceBox{X: .6, Y: .1, Width: .2, Height: .3}, "")
	require.NoError(t, err)
	x := &faceEmbeddingFixture{f: f, unmatched: unmatched}
	x.inputs, _, _, err = f.catalog.PhotoFacePersonNames(ctx, f.node.CurrentVersionID)
	require.NoError(t, err)
	server := httptest.NewTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "/v1/faces", r.URL.Path)
		var request embedclient.FaceRequest
		require.NoError(t, json.UnmarshalRead(r.Body, &request))
		require.NotEmpty(t, request.Image)
		x.calls++
		x.boxes = append(x.boxes, request.Boxes...)
		reply := embedclient.FaceResponse{Model: embedclient.FaceModel, Revision: embedclient.FaceRevision, Recipe: embedclient.FaceRecipe, Data: make([]embedclient.FaceResult, len(request.Boxes))}
		for i := range reply.Data {
			reply.Data[i].Index = i
			if !x.unmatched && (i == 0 || x.matchAll) {
				reply.Data[i].Embedding = make([]float32, 512)
				reply.Data[i].Embedding[0] = 1
				if x.vectorForBox != nil {
					reply.Data[i].Embedding = x.vectorForBox(request.Boxes[i])
				}
			}
		}
		require.NoError(t, json.MarshalWrite(w, reply, json.FormatNilSliceAsNull(true)))
	}))
	server.Start()
	profile, descriptor, err := config.LocalFaces()
	require.NoError(t, err)
	provider, err := localface.New(descriptor, func(context.Context) (string, error) { return server.URL, nil })
	require.NoError(t, err)
	service, err := NewService(ctx, ServiceConfig{Catalog: f.catalog, Blobs: f.blobs, Gate: newTestOperationGate(), SpoolDirectory: t.TempDir(), Lifecycle: ctx, Principal: "synthetic-operator", Scope: "document-processing", Profiles: map[string]ProfileConfig{config.LocalFacesProfile: {Profile: profile, EmbeddingProviders: map[string]document.EmbeddingProvider{config.LocalFacesProfile: provider}, EmbeddingClassifiers: map[string]func(error) (EmbeddingProviderFailure, time.Duration){config.LocalFacesProfile: func(error) (EmbeddingProviderFailure, time.Duration) { return EmbeddingProviderPermanent, 0 }}}}})
	require.NoError(t, err)
	x.service = service
	x.worker, err = NewEmbeddingWorker(EmbeddingWorkerConfig{Catalog: f.catalog, Authority: f.catalog, Blobs: f.blobs, GenerationBlobs: f.blobs, Runtime: service.EmbeddingRuntimes(), Gate: newTestOperationGate(), Owner: "face-test", RetryLimit: 1, RetryBaseDelay: time.Millisecond, MaxRetryDelay: time.Second, AttemptLifetime: time.Minute, MaxRows: 100, MaxDimensions: 512, MaxVectorBlobBytes: 1 << 20, LeaseDuration: time.Minute, IdleDelay: time.Millisecond, DescriptorFingerprints: []string{descriptor.Fingerprint}})
	require.NoError(t, err)
	return x
}
func TestFaceEmbeddingConsentBoxesAndNilResults(t *testing.T) {
	for _, unmatched := range []bool{false, true} {
		t.Run(map[bool]string{false: "mixed", true: "all nil"}[unmatched], func(t *testing.T) {
			x := newFaceEmbeddingFixture(t, unmatched)
			ctx := t.Context()
			n, err := x.worker.ScanOnce(ctx)
			require.NoError(t, err)
			require.Zero(t, n)
			require.Zero(t, x.calls)
			setup := FaceSetup{Service: x.service, Startup: func(context.Context) error { return nil }}
			plan, err := setup.Plan(ctx)
			require.NoError(t, err)
			require.Zero(t, x.calls)
			_, err = setup.Apply(ctx, "stale")
			require.ErrorIs(t, err, ErrPlanChanged)
			_, err = setup.Apply(ctx, plan.Fingerprint)
			require.NoError(t, err)
			x.worker.reconcileAt = time.Time{}
			n, err = x.worker.ScanOnce(ctx)
			require.NoError(t, err)
			require.Equal(t, 1, n)
			require.Equal(t, 1, x.calls)
			expected := make([]embedclient.FaceBox, len(x.inputs))
			for i, f := range x.inputs {
				expected[i] = embedclient.FaceBox(f.Box)
			}
			require.Equal(t, expected, x.boxes)
			faces, _, _, err := x.f.catalog.PhotoFacePersonNames(ctx, x.f.node.CurrentVersionID)
			require.NoError(t, err)
			require.Equal(t, x.inputs, faces)
			sets, err := x.f.catalog.FaceMatchingInputs(ctx)
			require.NoError(t, err)
			if unmatched {
				require.Empty(t, sets.Sets)
			} else {
				require.Len(t, sets.Sets, 1)
				require.Equal(t, 1, sets.Sets[0].Set.VectorSet.RowCount)
				require.Len(t, sets.Sets[0].Set.InputGeneration.Inputs, 2)
			}
			x.worker.reconcileAt = time.Time{}
			x.worker.reconcile = embeddingReconcileState{}
			n, err = x.worker.ScanOnce(ctx)
			require.NoError(t, err)
			require.Zero(t, n)
			require.Equal(t, 1, x.calls)
		})
	}
}
func TestFaceEmbeddingAddedBoxFencesVectors(t *testing.T) {
	x := newFaceEmbeddingFixture(t, false)
	ctx := t.Context()
	x.f.grant(t, x.service, config.LocalFacesProfile)
	_, err := x.worker.ScanOnce(ctx)
	require.NoError(t, err)
	sets, err := x.f.catalog.FaceMatchingInputs(ctx)
	require.NoError(t, err)
	require.Len(t, sets.Sets, 1)
	// Another face invalidates the whole input digest before the next inference.
	_, _, err = x.f.catalog.CreatePhotoFace(ctx, x.f.node.CurrentVersionID, uuid.New().String(), document.FaceBox{X: .4, Y: .5, Width: .2, Height: .2}, "")
	require.NoError(t, err)
	sets, err = x.f.catalog.FaceMatchingInputs(ctx)
	require.NoError(t, err)
	require.Empty(t, sets.Sets)
	require.True(t, slices.ContainsFunc(x.inputs, func(f store.PhotoFace) bool { return f.DetectorFingerprint != "" }))
}

func TestFaceMissingPreviewDoesNotBlockOtherPhotos(t *testing.T) {
	x := newFaceEmbeddingFixture(t, false)
	ctx := t.Context()
	version := x.f.addPhoto(t, "healthy.jpg", 720)
	_, _, err := x.f.catalog.CreatePhotoFace(ctx, version, uuid.New().String(), document.FaceBox{Width: .2, Height: .2}, "")
	require.NoError(t, err)
	x.f.grant(t, x.service, config.LocalFacesProfile)
	quality, err := document.CurrentPhotoQualityFingerprints()
	require.NoError(t, err)
	generation, err := x.f.catalog.VisualPreviewGenerationByRecipe(ctx, x.f.node.CurrentVersionID, quality.GridRecipe)
	require.NoError(t, err)
	hash := generation.Preview.Output.BlobSHA256
	source, err := x.f.blobs.OpenContext(ctx, hash)
	require.NoError(t, err)
	data, err := io.ReadAll(source)
	require.NoError(t, err)
	require.NoError(t, source.Close())
	require.NoError(t, x.f.blobs.Remove(hash))
	n, err := x.worker.ScanOnce(ctx)
	require.NoError(t, err)
	require.Equal(t, 1, n)
	_, err = x.f.blobs.WriteDetailedContext(ctx, bytes.NewReader(data))
	require.NoError(t, err)
	x.worker.reconcileAt = time.Time{}
	n, err = x.worker.ScanOnce(ctx)
	require.NoError(t, err)
	require.Equal(t, 1, n)
}

func TestFaceUnmatchedNamingSurvivesUnavailableSiblingVectors(t *testing.T) {
	x := newFaceEmbeddingFixture(t, false)
	ctx := t.Context()
	_, err := x.f.catalog.CreatePerson(ctx, "Ada", "operator")
	require.NoError(t, err)
	x.f.grant(t, x.service, config.LocalFacesProfile)
	_, err = x.worker.ScanOnce(ctx)
	require.NoError(t, err)
	sets, err := x.f.catalog.FaceMatchingInputs(ctx)
	require.NoError(t, err)
	require.Len(t, sets.Sets, 1)
	// The second inference row is explicitly unmatched.
	query := x.inputs[1].FaceID
	options := store.PeopleSearchOptions{FaceID: query, FaceReader: x.f.blobs, RecentFirst: true}
	rows, _, err := x.f.catalog.PeopleByDisplayName(ctx, "", "", "", 24, false, options)
	require.NoError(t, err)
	require.Len(t, rows, 1)
	require.NoError(t, x.f.blobs.Remove(sets.Sets[0].Set.VectorSet.PayloadBlobHash))
	rows, _, err = x.f.catalog.PeopleByDisplayName(ctx, "", "", "", 24, false, options)
	require.NoError(t, err)
	require.Len(t, rows, 1)
	require.Equal(t, "Ada", rows[0].DisplayName)
}

func TestFaceSuggestionsRebuildAndRestoreDecisions(t *testing.T) {
	x := newFaceEmbeddingFixture(t, false)
	x.matchAll = true
	ctx := t.Context()
	x.f.grant(t, x.service, config.LocalFacesProfile)
	_, err := x.worker.ScanOnce(ctx)
	require.NoError(t, err)
	require.NoError(t, x.service.rebuildFaceSuggestions(ctx))
	groups, _, err := x.f.catalog.FaceCandidates(ctx, 24, 0)
	require.NoError(t, err)
	require.Len(t, groups, 1)
	require.Len(t, groups[0].Faces, 2)
	first := groups[0].Faces[0]
	_, err = x.f.catalog.DecidePersonCandidate(ctx, store.CandidateDecision{CandidateID: groups[0].CandidateID, ExpectedRevision: groups[0].Revision, Action: "reject", FaceOnly: true})
	require.NoError(t, err)
	require.NoError(t, x.service.rebuildFaceSuggestions(ctx))
	groups, _, err = x.f.catalog.FaceCandidates(ctx, 24, 0)
	require.NoError(t, err)
	require.Empty(t, groups)
	var backup bytes.Buffer
	require.NoError(t, x.f.catalog.ExportMetadata(ctx, &backup))
	target, err := store.Open(ctx, filepath.Join(t.TempDir(), "restored.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, target.Close()) })
	require.NoError(t, target.ImportMetadata(ctx, bytes.NewReader(backup.Bytes())))
	require.NoError(t, target.ValidateMetadata(ctx))
	original := x.service.profiles[config.LocalFacesProfile]
	restored, err := NewService(ctx, ServiceConfig{Catalog: target, Blobs: x.f.blobs, Gate: newTestOperationGate(), SpoolDirectory: t.TempDir(), Lifecycle: ctx, Principal: x.service.principal, Scope: x.service.scope, Profiles: map[string]ProfileConfig{config.LocalFacesProfile: {Profile: original.portable, EmbeddingProviders: original.embedders, EmbeddingClassifiers: map[string]func(error) (EmbeddingProviderFailure, time.Duration){config.LocalFacesProfile: original.embeddingRuntimes[config.LocalFacesProfile].Classify}}}})
	require.NoError(t, err)
	require.NoError(t, restored.rebuildFaceSuggestions(ctx))
	groups, _, err = target.FaceCandidates(ctx, 24, 0)
	require.NoError(t, err)
	require.Empty(t, groups)
	faces, _, _, err := target.PhotoFacePersonNames(ctx, first.ContentVersionID)
	require.NoError(t, err)
	require.True(t, slices.ContainsFunc(faces, func(f store.PhotoFace) bool {
		return f.FaceID == first.FaceID && !f.Dismissed
	}))
	_, err = target.DecidePhotoFace(ctx, first.ContentVersionID, store.PhotoFaceDecisionRequest{FaceID: first.FaceID, Revision: faces[0].Revision, Action: "name", PersonName: "Ada"})
	require.NoError(t, err)
	require.NoError(t, restored.rebuildFaceSuggestions(ctx))
	groups, _, err = target.FaceCandidates(ctx, 24, 0)
	require.NoError(t, err)
	require.Len(t, groups, 1)
	require.Equal(t, "Ada", groups[0].DisplayName)
	require.Len(t, groups[0].Faces, 1)
	require.NotEqual(t, first.FaceID, groups[0].Faces[0].FaceID)
	require.Equal(t, 1, x.calls)
}

func TestFacePeopleRankingPrecedesPagingAndKeepsNamesakes(t *testing.T) {
	x := newFaceEmbeddingFixture(t, false)
	x.matchAll = true
	ctx := t.Context()
	var queryFace store.PhotoFace
	for _, f := range x.inputs {
		if f.DetectorFingerprint != "" {
			queryFace = f
		}
	}
	for i, name := range []string{"Ada", "Alex", "ALEX", "Zoe"} {
		p, err := x.f.catalog.CreatePerson(ctx, name, "operator")
		require.NoError(t, err)
		_, _, err = x.f.catalog.CreatePhotoFace(ctx, x.f.node.CurrentVersionID, uuid.New().String(), document.FaceBox{X: float64(i) * .2, Y: .6, Width: .1, Height: .1}, p.PersonID)
		require.NoError(t, err)
	}
	x.vectorForBox = func(box embedclient.FaceBox) []float32 {
		v := make([]float32, 512)
		if box.Y < .5 || box.X > .5 {
			v[0] = 1
		} else if box.X > .1 && box.X < .3 {
			v[1] = 1
		} else {
			v[0] = -1
		}
		return v
	}
	x.f.grant(t, x.service, config.LocalFacesProfile)
	_, err := x.worker.ScanOnce(ctx)
	require.NoError(t, err)
	options := store.PeopleSearchOptions{FaceID: queryFace.FaceID, FaceReader: x.f.blobs, RecentFirst: true}
	page, _, err := x.f.catalog.PeopleByDisplayName(ctx, "", "", "", 2, false, options)
	require.NoError(t, err)
	require.Len(t, page, 2)
	require.Equal(t, "Zoe", page[0].DisplayName)
	require.Equal(t, "Alex", page[1].DisplayName)
	options.AfterRecent = page[1].RecentRank
	next, _, err := x.f.catalog.PeopleByDisplayName(ctx, "", page[1].DisplayNameFolded, page[1].PersonID, 2, false, options)
	require.NoError(t, err)
	require.Len(t, next, 2)
	require.Equal(t, "ALEX", next[0].DisplayName)
	require.Equal(t, "Ada", next[1].DisplayName)
	sets, err := x.f.catalog.FaceMatchingInputs(ctx)
	require.NoError(t, err)
	require.Len(t, sets.Sets, 1)
	require.NoError(t, x.f.blobs.Remove(sets.Sets[0].Set.VectorSet.PayloadBlobHash))
	// A cold catalog cannot continue ranking after its source payload disappears;
	// an already decoded immutable payload can still serve a warm request.
	var metadata bytes.Buffer
	require.NoError(t, x.f.catalog.ExportMetadata(ctx, &metadata))
	cold, err := store.Open(ctx, filepath.Join(t.TempDir(), "cold.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, cold.Close()) })
	require.NoError(t, cold.ImportMetadata(ctx, bytes.NewReader(metadata.Bytes())))
	_, _, err = cold.PeopleByDisplayName(ctx, "", page[1].DisplayNameFolded, page[1].PersonID, 2, false, options)
	require.ErrorIs(t, err, store.ErrStaleRevision)
}

func TestFacePeopleRankingObeysHiddenScopeAndRenaming(t *testing.T) {
	x := newFaceEmbeddingFixture(t, false)
	x.matchAll = true
	ctx := t.Context()
	person, err := x.f.catalog.CreatePerson(ctx, "Ada", "operator")
	require.NoError(t, err)
	named := x.inputs[0]
	_, err = x.f.catalog.DecidePhotoFace(ctx, named.ContentVersionID, store.PhotoFaceDecisionRequest{FaceID: named.FaceID, Revision: named.Revision, Action: "name", PersonID: person.PersonID})
	require.NoError(t, err)
	version := x.f.addPhoto(t, "visible.jpg", 720)
	query, _, err := x.f.catalog.CreatePhotoFace(ctx, version, uuid.New().String(), document.FaceBox{Width: .2, Height: .2}, "")
	require.NoError(t, err)
	x.f.grant(t, x.service, config.LocalFacesProfile)
	_, err = x.worker.ScanOnce(ctx)
	require.NoError(t, err)
	require.NoError(t, x.f.catalog.SetupPhotoHidden(ctx, "synthetic-passcode"))
	asset, err := x.f.catalog.PhotoAssetForNode(ctx, x.f.node.ID)
	require.NoError(t, err)
	_, err = x.f.catalog.SetPhotoAssetHidden(ctx, asset.ID, asset.Revision, true)
	require.NoError(t, err)
	token, _, err := x.f.catalog.UnlockPhotoHidden(ctx, "synthetic-passcode")
	require.NoError(t, err)
	unlocked := store.WithPhotoHiddenToken(ctx, token)
	options := store.PeopleSearchOptions{FaceID: query.FaceID, FaceReader: x.f.blobs, RecentFirst: true}
	rows, _, err := x.f.catalog.PeopleByDisplayName(unlocked, "", "", "", 24, true, options)
	require.NoError(t, err)
	require.Len(t, rows, 1)
	require.Equal(t, "Ada", rows[0].DisplayName)
	rows, _, err = x.f.catalog.PeopleByDisplayName(ctx, "", "", "", 24, true, options)
	require.NoError(t, err)
	require.Empty(t, rows)
	_, err = x.f.catalog.UpdatePerson(unlocked, person.PersonID, person.Revision, "Renamed")
	require.NoError(t, err)
	rows, _, err = x.f.catalog.PeopleByDisplayName(unlocked, "", "", "", 24, true, options)
	require.NoError(t, err)
	require.Len(t, rows, 1)
	require.Equal(t, "Renamed", rows[0].DisplayName)
}

// Counting the real blob boundary catches repeated payload decoding across
// picker searches without replacing the ranking or matching implementation.
type facePayloadReads struct {
	store.RenditionBlobReader
	opens int
}

func (r *facePayloadReads) OpenStreamContext(ctx context.Context, hash string) (packstore.VerifiedReadCloser, int64, error) {
	r.opens++
	return r.RenditionBlobReader.OpenStreamContext(ctx, hash)
}
func TestFaceMatchingSnapshotAndPayloadReuse(t *testing.T) {
	x := newFaceEmbeddingFixture(t, false)
	x.matchAll = true
	ctx := t.Context()
	x.f.grant(t, x.service, config.LocalFacesProfile)
	_, err := x.worker.ScanOnce(ctx)
	require.NoError(t, err)
	snapshot, err := x.f.catalog.FaceMatchingInputs(ctx)
	require.NoError(t, err)
	reader := &facePayloadReads{RenditionBlobReader: x.f.blobs}
	options := store.PeopleSearchOptions{FaceID: x.inputs[0].FaceID, FaceReader: reader, RecentFirst: true}
	_, _, err = x.f.catalog.PeopleByDisplayName(ctx, "", "", "", 100, false, options)
	require.NoError(t, err)
	require.Equal(t, 1, reader.opens)
	for _, query := range []string{"A", "Ad", "Ada"} {
		_, _, err = x.f.catalog.PeopleByDisplayName(ctx, query, "", "", 100, false, options)
		require.NoError(t, err)
	}
	require.Equal(t, 1, reader.opens)
	// A document write cannot fence a derivation from unchanged photographed faces.
	_, err = x.f.catalog.CreateFile(ctx, x.f.catalog.RootID(), "unrelated.txt", x.f.node.BlobHash, x.f.node.Size, "text/plain")
	require.NoError(t, err)
	unchanged, err := x.f.catalog.FaceMatchingInputs(ctx)
	require.NoError(t, err)
	require.Equal(t, snapshot.Signature, unchanged.Signature)
	vectors, err := x.f.catalog.FaceMatchingVectors(ctx, reader, snapshot, true)
	require.NoError(t, err)
	candidates, err := buildFaceSuggestions(vectors)
	require.NoError(t, err)
	require.NoError(t, x.f.catalog.ReplaceFaceSuggestions(ctx, snapshot.Signature, candidates))
	published, err := x.f.catalog.FaceMatchingInputs(ctx)
	require.NoError(t, err)
	require.Equal(t, snapshot.Signature, published.Signature)
	require.NoError(t, x.service.rebuildFaceSuggestions(ctx))
	require.NoError(t, x.service.rebuildFaceSuggestions(ctx))
	_, _, err = x.f.catalog.PeopleByDisplayName(ctx, "", "", "", 100, false, options)
	require.NoError(t, err)
	require.Equal(t, 1, reader.opens)
	// Assignment changes invalidate the signature and immediately change centroids,
	// while the immutable payload remains shared between grouping and ranking.
	_, err = x.f.catalog.DecidePhotoFace(ctx, x.inputs[0].ContentVersionID, store.PhotoFaceDecisionRequest{FaceID: x.inputs[0].FaceID, Revision: x.inputs[0].Revision, Action: "name", PersonName: "Ada"})
	require.NoError(t, err)
	changed, err := x.f.catalog.FaceMatchingInputs(ctx)
	require.NoError(t, err)
	require.NotEqual(t, snapshot.Signature, changed.Signature)
	require.ErrorIs(t, x.f.catalog.ReplaceFaceSuggestions(ctx, snapshot.Signature, candidates), store.ErrStaleRevision)
	vectors, err = x.f.catalog.FaceMatchingVectors(ctx, reader, changed, true)
	require.NoError(t, err)
	centroids := store.FacePersonCentroids(vectors)
	require.Len(t, centroids, 1)
	require.Equal(t, "Ada", centroids[0].Name)
	require.Equal(t, 1, reader.opens)
}
