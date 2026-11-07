package processing

import (
	"bytes"
	"context"
	"encoding/json/v2"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"
	"uuid"

	"github.com/stretchr/testify/assert"
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
	onRequest    func()
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
		if x.onRequest != nil {
			x.onRequest()
		}
		assert.Equal(t, "/v1/faces", r.URL.Path)
		var request embedclient.FaceRequest
		if !assert.NoError(t, json.UnmarshalRead(r.Body, &request)) {
			return
		}
		assert.NotEmpty(t, request.Image)
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
		assert.NoError(t, json.MarshalWrite(w, reply, json.FormatNilSliceAsNull(true)))
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
func TestFaceEmbeddingBoxesAndNilResults(t *testing.T) {
	t.Parallel()
	for _, unmatched := range []bool{false, true} {
		t.Run(map[bool]string{false: "mixed", true: "all nil"}[unmatched], func(t *testing.T) {
			x := newFaceEmbeddingFixture(t, unmatched)
			ctx := t.Context()
			x.f.grant(t, x.service, config.LocalFacesProfile)
			x.worker.reconcileAt = time.Time{}
			n, err := x.worker.ScanOnce(ctx)
			require.NoError(t, err)
			require.Equal(t, 1, n)
			require.Equal(t, 1, x.calls)
			coverage, err := x.service.Coverage(ctx, config.LocalFacesProfile, SourceFence{VaultUID: x.f.catalog.VaultID(), ContentVersionIDs: []string{x.f.node.CurrentVersionID}})
			require.NoError(t, err)
			require.Len(t, coverage.Embeddings, 1)
			require.Equal(t, 1, coverage.Embeddings[0].Complete, "aligned and all-unmatched attempts both finish processing")
			require.Zero(t, coverage.Embeddings[0].Unavailable)
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
	t.Parallel()
	x := newFaceEmbeddingFixture(t, false)
	ctx := t.Context()
	x.f.grant(t, x.service, config.LocalFacesProfile)
	_, err := x.worker.ScanOnce(ctx)
	require.NoError(t, err)
	sets, err := x.f.catalog.FaceMatchingInputs(ctx)
	require.NoError(t, err)
	require.Len(t, sets.Sets, 1)
	// Adding a face makes the previous vectors ineligible for matching.
	_, _, err = x.f.catalog.CreatePhotoFace(ctx, x.f.node.CurrentVersionID, uuid.New().String(), document.FaceBox{X: .4, Y: .5, Width: .2, Height: .2}, "")
	require.NoError(t, err)
	sets, err = x.f.catalog.FaceMatchingInputs(ctx)
	require.NoError(t, err)
	require.Empty(t, sets.Sets)
}

func TestFaceMissingPreviewDoesNotBlockOtherPhotos(t *testing.T) {
	t.Parallel()
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
	_, err = x.worker.ScanOnce(ctx)
	require.NoError(t, err)
	sets, err := x.f.catalog.FaceMatchingInputs(ctx)
	require.NoError(t, err)
	require.Len(t, sets.Sets, 1)
	require.Equal(t, version, sets.Sets[0].Set.ContentVersionID)
	_, err = x.f.blobs.WriteDetailedContext(ctx, bytes.NewReader(data))
	require.NoError(t, err)
	x.worker.reconcileAt = time.Time{}
	_, err = x.worker.ScanOnce(ctx)
	require.NoError(t, err)
	sets, err = x.f.catalog.FaceMatchingInputs(ctx)
	require.NoError(t, err)
	require.Len(t, sets.Sets, 2)
	require.ElementsMatch(t, []string{version, x.f.node.CurrentVersionID}, []string{sets.Sets[0].Set.ContentVersionID, sets.Sets[1].Set.ContentVersionID})
}

func TestFaceNamingAndRankingContinuationWithUnavailableVectors(t *testing.T) {
	t.Parallel()
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
	matchedOptions := options
	matchedOptions.FaceID = x.inputs[0].FaceID
	matched, _, err := x.f.catalog.PeopleByDisplayName(ctx, "", "", "", 24, false, matchedOptions)
	require.NoError(t, err)
	require.Len(t, matched, 1)
	matchedOptions.AfterRecent = matched[0].RecentRank
	require.NoError(t, x.f.blobs.Remove(sets.Sets[0].Set.VectorSet.PayloadBlobHash))
	var metadata bytes.Buffer
	require.NoError(t, x.f.catalog.ExportMetadata(ctx, &metadata))
	cold, err := store.Open(ctx, filepath.Join(t.TempDir(), "cold.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, cold.Close()) })
	require.NoError(t, cold.ImportMetadata(ctx, bytes.NewReader(metadata.Bytes())))
	rows, _, err := cold.PeopleByDisplayName(ctx, "", "", "", 24, false, options)
	require.NoError(t, err)
	require.Len(t, rows, 1)
	require.Equal(t, "Ada", rows[0].DisplayName)
	_, _, err = cold.PeopleByDisplayName(ctx, "", matched[0].DisplayNameFolded, matched[0].PersonID, 24, false, matchedOptions)
	require.ErrorIs(t, err, store.ErrStaleRevision, "a matched face cannot continue ranking without its source payload")
}

func TestFacePeopleRankingPrecedesPagingAndKeepsNamesakes(t *testing.T) {
	t.Parallel()
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
}

func TestFacePeopleRankingObeysHiddenScope(t *testing.T) {
	t.Parallel()
	x := newFaceEmbeddingFixture(t, false)
	x.matchAll = true
	ctx := t.Context()
	ada, err := x.f.catalog.CreatePerson(ctx, "Ada", "operator")
	require.NoError(t, err)
	beth, err := x.f.catalog.CreatePerson(ctx, "Beth", "operator")
	require.NoError(t, err)
	named := x.inputs[0]
	_, err = x.f.catalog.DecidePhotoFace(ctx, named.ContentVersionID, store.PhotoFaceDecisionRequest{FaceID: named.FaceID, Revision: named.Revision, Action: "name", PersonID: ada.PersonID})
	require.NoError(t, err)
	version := x.f.addPhoto(t, "visible-query.jpg", 720)
	query, _, err := x.f.catalog.CreatePhotoFace(ctx, version, uuid.New().String(), document.FaceBox{Width: .2, Height: .2}, "")
	require.NoError(t, err)
	bethBox := document.FaceBox{X: .7, Y: .7, Width: .2, Height: .2}
	_, _, err = x.f.catalog.CreatePhotoFace(ctx, version, uuid.New().String(), bethBox, beth.PersonID)
	require.NoError(t, err)
	hiddenVersion := x.f.addPhoto(t, "hidden-evidence.jpg", 721)
	for _, left := range []float64{.1, .6} {
		_, _, err = x.f.catalog.CreatePhotoFace(ctx, hiddenVersion, uuid.New().String(), document.FaceBox{X: left, Y: .6, Width: .2, Height: .2}, ada.PersonID)
		require.NoError(t, err)
	}
	x.vectorForBox = func(box embedclient.FaceBox) []float32 {
		values := make([]float32, 512)
		values[0] = 1
		if box == embedclient.FaceBox(named.Box) {
			values[0] = -1
		}
		if box == embedclient.FaceBox(bethBox) {
			values[0], values[1] = 0, 1
		}
		return values
	}
	x.f.grant(t, x.service, config.LocalFacesProfile)
	_, err = x.worker.ScanOnce(ctx)
	require.NoError(t, err)
	require.NoError(t, x.f.catalog.SetupPhotoHidden(ctx, "synthetic-passcode"))
	source, err := x.f.catalog.ContentVersionByID(ctx, hiddenVersion)
	require.NoError(t, err)
	asset, err := x.f.catalog.PhotoAssetForNode(ctx, source.NodeID)
	require.NoError(t, err)
	_, err = x.f.catalog.SetPhotoAssetHidden(ctx, asset.ID, asset.Revision, true)
	require.NoError(t, err)
	token, _, err := x.f.catalog.UnlockPhotoHidden(ctx, "synthetic-passcode")
	require.NoError(t, err)
	options := store.PeopleSearchOptions{FaceID: query.FaceID, FaceReader: x.f.blobs, RecentFirst: true}
	unlocked := store.WithPhotoHiddenToken(ctx, token)
	rows, _, err := x.f.catalog.PeopleByDisplayName(unlocked, "", "", "", 24, true, options)
	require.NoError(t, err)
	require.Len(t, rows, 2)
	require.Equal(t, ada.PersonID, rows[0].PersonID)
	rows, _, err = x.f.catalog.PeopleByDisplayName(ctx, "", "", "", 24, true, options)
	require.NoError(t, err)
	require.Len(t, rows, 2)
	require.Equal(t, beth.PersonID, rows[0].PersonID, "both people stay visible, but Hidden evidence must stop affecting rank")
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

func TestFacePickerReusesPayloadAcrossNameQueries(t *testing.T) {
	t.Parallel()
	x := newFaceEmbeddingFixture(t, false)
	x.matchAll = true
	ctx := t.Context()
	named := x.inputs[0]
	_, err := x.f.catalog.DecidePhotoFace(ctx, named.ContentVersionID, store.PhotoFaceDecisionRequest{FaceID: named.FaceID, Revision: named.Revision, Action: "name", PersonName: "Ada"})
	require.NoError(t, err)
	x.f.grant(t, x.service, config.LocalFacesProfile)
	_, err = x.worker.ScanOnce(ctx)
	require.NoError(t, err)
	reader := &facePayloadReads{RenditionBlobReader: x.f.blobs}
	options := store.PeopleSearchOptions{FaceID: x.inputs[1].FaceID, FaceReader: reader, RecentFirst: true}
	_, _, err = x.f.catalog.PeopleByDisplayName(ctx, "", "", "", 100, false, options)
	require.NoError(t, err)
	require.Equal(t, 1, reader.opens)
	for _, query := range []string{"A", "Ad", "Ada"} {
		people, _, err := x.f.catalog.PeopleByDisplayName(ctx, query, "", "", 100, false, options)
		require.NoError(t, err)
		require.Len(t, people, 1)
	}
	require.Equal(t, 1, reader.opens)
}

func TestFacePickerLoadsSourceAndNamedEvidenceOnly(t *testing.T) {
	t.Parallel()
	x := newFaceEmbeddingFixture(t, false)
	x.matchAll = true
	ctx := t.Context()
	person, err := x.f.catalog.CreatePerson(ctx, "Ada", "operator")
	require.NoError(t, err)
	named := x.f.addPhoto(t, "named.jpg", 701)
	_, _, err = x.f.catalog.CreatePhotoFace(ctx, named, uuid.New().String(), document.FaceBox{Width: .2, Height: .2}, person.PersonID)
	require.NoError(t, err)
	unrelated := x.f.addPhoto(t, "unnamed.jpg", 702)
	_, _, err = x.f.catalog.CreatePhotoFace(ctx, unrelated, uuid.New().String(), document.FaceBox{Width: .2, Height: .2}, "")
	require.NoError(t, err)
	x.f.grant(t, x.service, config.LocalFacesProfile)
	count, err := x.worker.ScanOnce(ctx)
	require.NoError(t, err)
	require.Equal(t, 3, count)
	reader := &facePayloadReads{RenditionBlobReader: x.f.blobs}
	options := store.PeopleSearchOptions{FaceID: x.inputs[0].FaceID, FaceReader: reader, RecentFirst: true}
	people, _, err := x.f.catalog.PeopleByDisplayName(ctx, "", "", "", 24, false, options)
	require.NoError(t, err)
	require.Len(t, people, 1)
	require.Equal(t, person.PersonID, people[0].PersonID)
	require.Equal(t, 2, reader.opens, "the unrelated unnamed photo cannot affect person ranking")
	snapshot, err := x.f.catalog.FaceMatchingInputs(ctx)
	require.NoError(t, err)
	_, err = x.f.catalog.FaceMatchingVectors(ctx, reader, snapshot, true)
	require.NoError(t, err)
	require.Equal(t, 3, reader.opens, "grouping adds the unrelated photo's uncached payload")
	_, _, err = x.f.catalog.PeopleByDisplayName(ctx, "", "", "", 24, false, options)
	require.NoError(t, err)
	_, err = x.f.catalog.FaceMatchingVectors(ctx, reader, snapshot, true)
	require.NoError(t, err)
	require.Equal(t, 3, reader.opens, "picker subsets must preserve grouping's referenced payloads")
}

func TestVisibleFaceSuggestionUsesVisibleNamedEvidence(t *testing.T) {
	t.Parallel()
	x := newFaceEmbeddingFixture(t, false)
	x.matchAll = true
	x.vectorForBox = func(box embedclient.FaceBox) []float32 {
		values := make([]float32, 512)
		values[0] = 1
		if box.Y > .5 {
			values[0] = -1
		}
		return values
	}
	ctx := t.Context()
	person, err := x.f.catalog.CreatePerson(ctx, "Ada", "operator")
	require.NoError(t, err)
	_, err = x.f.catalog.DecidePhotoFace(ctx, x.inputs[0].ContentVersionID, store.PhotoFaceDecisionRequest{FaceID: x.inputs[0].FaceID, Revision: x.inputs[0].Revision, Action: "name", PersonID: person.PersonID})
	require.NoError(t, err)
	hiddenVersion := x.f.addPhoto(t, "private.jpg", 710)
	for _, left := range []float64{.1, .6} {
		_, _, err = x.f.catalog.CreatePhotoFace(ctx, hiddenVersion, uuid.New().String(), document.FaceBox{X: left, Y: .6, Width: .2, Height: .2}, person.PersonID)
		require.NoError(t, err)
	}
	hiddenQuery, _, err := x.f.catalog.CreatePhotoFace(ctx, hiddenVersion, uuid.New().String(), document.FaceBox{X: .4, Y: .6, Width: .2, Height: .2}, "")
	require.NoError(t, err)
	queryVersion := x.f.addPhoto(t, "query.jpg", 711)
	_, _, err = x.f.catalog.CreatePhotoFace(ctx, queryVersion, uuid.New().String(), document.FaceBox{Y: .6, Width: .2, Height: .2}, "")
	require.NoError(t, err)
	x.f.grant(t, x.service, config.LocalFacesProfile)
	count, err := x.worker.ScanOnce(ctx)
	require.NoError(t, err)
	require.Equal(t, 3, count)
	require.NoError(t, x.f.catalog.SetupPhotoHidden(ctx, "synthetic-passcode"))
	version, err := x.f.catalog.ContentVersionByID(ctx, hiddenVersion)
	require.NoError(t, err)
	asset, err := x.f.catalog.PhotoAssetForNode(ctx, version.NodeID)
	require.NoError(t, err)
	_, err = x.f.catalog.SetPhotoAssetHidden(ctx, asset.ID, asset.Revision, true)
	require.NoError(t, err)
	require.NoError(t, x.service.rebuildFaceSuggestions(ctx))
	page, total, err := x.f.catalog.FaceCandidates(ctx, 24, 0)
	require.NoError(t, err)
	require.EqualValues(t, 1, total)
	require.Len(t, page, 1)
	require.Equal(t, person.PersonID, page[0].SuggestedPersonID)
	require.Len(t, page[0].Faces, 1)
	require.Equal(t, x.inputs[1].FaceID, page[0].Faces[0].FaceID, "visible matches must agree with the visible-only naming evidence")
	token, _, err := x.f.catalog.UnlockPhotoHidden(ctx, "synthetic-passcode")
	require.NoError(t, err)
	page, total, err = x.f.catalog.FaceCandidates(store.WithPhotoHiddenToken(ctx, token), 24, 0)
	require.NoError(t, err)
	require.EqualValues(t, 2, total)
	var hiddenFaces []string
	for _, candidate := range page {
		for _, face := range candidate.Faces {
			if face.Hidden {
				hiddenFaces = append(hiddenFaces, face.FaceID)
			}
		}
	}
	require.Equal(t, []string{hiddenQuery.FaceID}, hiddenFaces, "Hidden-source matching remains available after unlock")
}

func TestVisibleUnnamedGroupSurvivesHiddenMatch(t *testing.T) {
	t.Parallel()
	x := newFaceEmbeddingFixture(t, false)
	x.matchAll = true
	ctx := t.Context()
	hiddenVersion := x.f.addPhoto(t, "private-match.jpg", 730)
	_, _, err := x.f.catalog.CreatePhotoFace(ctx, hiddenVersion, uuid.New().String(), document.FaceBox{Width: .2, Height: .2}, "")
	require.NoError(t, err)
	x.f.grant(t, x.service, config.LocalFacesProfile)
	count, err := x.worker.ScanOnce(ctx)
	require.NoError(t, err)
	require.Equal(t, 2, count)
	version, err := x.f.catalog.ContentVersionByID(ctx, hiddenVersion)
	require.NoError(t, err)
	asset, err := x.f.catalog.PhotoAssetForNode(ctx, version.NodeID)
	require.NoError(t, err)
	require.NoError(t, x.f.catalog.SetupPhotoHidden(ctx, "synthetic-passcode"))
	_, err = x.f.catalog.SetPhotoAssetHidden(ctx, asset.ID, asset.Revision, true)
	require.NoError(t, err)
	require.NoError(t, x.service.rebuildFaceSuggestions(ctx))
	page, total, err := x.f.catalog.FaceCandidates(ctx, 24, 0)
	require.NoError(t, err)
	require.EqualValues(t, 1, total)
	require.Len(t, page, 1)
	require.Len(t, page[0].Faces, 2)
	require.ElementsMatch(t, []string{x.inputs[0].FaceID, x.inputs[1].FaceID}, []string{page[0].Faces[0].FaceID, page[0].Faces[1].FaceID})
}

func TestFaceCoverageReportsRunningAndCompletedVectors(t *testing.T) {
	t.Parallel()
	x := newFaceEmbeddingFixture(t, false)
	ctx := t.Context()
	x.f.grant(t, x.service, config.LocalFacesProfile)
	fence := SourceFence{VaultUID: x.f.catalog.VaultID(), ContentVersionIDs: []string{x.f.node.CurrentVersionID}}
	var running Coverage
	var coverageErr error
	x.onRequest = func() { running, coverageErr = x.service.Coverage(ctx, config.LocalFacesProfile, fence) }
	_, err := x.worker.ScanOnce(ctx)
	require.NoError(t, err)
	require.NoError(t, coverageErr)
	require.Len(t, running.Embeddings, 1)
	assert.Equal(t, 1, running.Embeddings[0].Rebuilding, "inference is running for this version")
	complete, err := x.service.Coverage(ctx, config.LocalFacesProfile, fence)
	require.NoError(t, err)
	require.Len(t, complete.Embeddings, 1)
	assert.Equal(t, 1, complete.Embeddings[0].Complete, "the current face vectors have published")
	_, _, err = x.f.catalog.CreatePhotoFace(ctx, x.f.node.CurrentVersionID, uuid.New().String(), document.FaceBox{X: .2, Width: .1, Height: .1}, "")
	require.NoError(t, err)
	stale, err := x.service.Coverage(ctx, config.LocalFacesProfile, fence)
	require.NoError(t, err)
	assert.Zero(t, stale.Embeddings[0].Complete, "a changed face generation must stop counting as complete")
}

func TestFacePlanDisclosesScopeAndRequiresCurrentConsent(t *testing.T) {
	t.Parallel()
	x := newFaceEmbeddingFixture(t, false)
	selector := Selector{NodeID: x.f.node.ID, ContentVersionID: x.f.node.CurrentVersionID, Profile: config.LocalFacesProfile}
	plan, err := x.service.Plan(t.Context(), selector)
	require.NoError(t, err)
	require.Contains(t, plan.Estimate.Scope, "throughout this vault")
	require.Contains(t, plan.Estimate.Scope, "Hidden")
	setup := FaceSetup{Service: x.service, Startup: func(context.Context) error { return nil }}
	reviewed, err := setup.Plan(t.Context())
	require.NoError(t, err)
	require.Equal(t, plan.Estimate.Scope, reviewed.Scope)
	n, err := x.worker.ScanOnce(t.Context())
	require.NoError(t, err)
	require.Zero(t, n)
	require.Zero(t, x.calls)
	_, err = setup.Apply(t.Context(), "stale")
	require.ErrorIs(t, err, ErrPlanChanged)
	n, err = x.worker.ScanOnce(t.Context())
	require.NoError(t, err)
	require.Zero(t, n)
	require.Zero(t, x.calls)
	_, err = setup.Apply(t.Context(), reviewed.Fingerprint)
	require.NoError(t, err)
	x.worker.reconcileAt = time.Time{}
	n, err = x.worker.ScanOnce(t.Context())
	require.NoError(t, err)
	require.Equal(t, 1, n)
	require.Equal(t, 1, x.calls)
}

func TestFaceAggregateStatusIncludesLiveJobs(t *testing.T) {
	t.Parallel()
	for _, invalid := range []bool{false, true} {
		t.Run(map[bool]string{false: "completed", true: "partial"}[invalid], func(t *testing.T) {
			x := newFaceEmbeddingFixture(t, false)
			if invalid {
				x.vectorForBox = func(embedclient.FaceBox) []float32 { return []float32{1} }
			}
			selector := Selector{NodeID: x.f.node.ID, ContentVersionID: x.f.node.CurrentVersionID, Profile: config.LocalFacesProfile}
			plan, err := x.service.Plan(t.Context(), selector)
			require.NoError(t, err)
			var queued, running Status
			var queuedErr, runningErr error
			var id string
			x.onRequest = func() { running, runningErr = x.service.Status(t.Context(), id) }
			job, err := x.service.StartWithProgress(t.Context(), StartRequest{Selector: selector, PlanFingerprint: plan.Fingerprint, Consent: true}, func(job Job) {
				id = job.ID
				queued, queuedErr = x.service.Status(t.Context(), id)
			})
			require.NoError(t, err)
			require.NotEmpty(t, id)
			require.NoError(t, queuedErr)
			require.NoError(t, runningErr)
			assert.Equal(t, "queued", queued.State)
			assert.Equal(t, []string{id}, queued.EmbeddingJobIDs)
			assert.Equal(t, "running", running.State)
			assert.Equal(t, []string{id}, running.EmbeddingJobIDs)
			after, err := x.service.Status(t.Context(), job.ID)
			require.NoError(t, err)
			if invalid {
				assert.Equal(t, "partial", after.State)
				assert.NotEmpty(t, after.FailureCode)
			} else {
				assert.Equal(t, "completed", after.State)
				assert.Equal(t, 1, after.CompletedBindings)
			}
		})
	}
}
