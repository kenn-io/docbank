package processing

import (
	"bytes"
	"context"
	"encoding/json/v2"
	"fmt"
	"image/color"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"uuid"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/document/localface"
	"go.kenn.io/docbank/document/media/mediatest"
	"go.kenn.io/docbank/internal/config"
	"go.kenn.io/docbank/internal/store"
	"go.kenn.io/docbank/internal/vectorworker"
	docsqlite "go.kenn.io/docbank/sqlite"
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
func (x *faceEmbeddingFixture) enable(t *testing.T) {
	t.Helper()
	setup := FaceSetup{Service: x.service, Startup: func(context.Context) error { return nil }}
	plan, err := setup.Plan(t.Context())
	require.NoError(t, err)
	_, err = setup.Apply(t.Context(), plan.Fingerprint)
	require.NoError(t, err)
}

func TestFaceEmbeddingBoxesAndNilResults(t *testing.T) {
	t.Parallel()
	for _, unmatched := range []bool{false, true} {
		t.Run(map[bool]string{false: "mixed", true: "all nil"}[unmatched], func(t *testing.T) {
			t.Parallel()
			x := newFaceEmbeddingFixture(t, unmatched)
			ctx := t.Context()
			x.enable(t)
			x.worker.reconcileAt = time.Time{}
			var jobID string
			observed := &countedReconcileCatalog{Store: x.f.catalog, afterValidation: func(claim EmbeddingWorkClaim) { jobID = claim.AttemptID }}
			x.worker.catalog = observed
			n, err := x.worker.ScanOnce(ctx)
			require.NoError(t, err)
			require.Equal(t, 1, n)
			require.Equal(t, 1, x.calls)
			expected := make([]embedclient.FaceBox, len(x.inputs))
			for i, f := range x.inputs {
				expected[i] = embedclient.FaceBox(f.Box)
			}
			require.Equal(t, expected, x.boxes)
			sets, err := x.f.catalog.FaceMatchingInputs(ctx)
			require.NoError(t, err)
			if unmatched {
				job, err := x.f.catalog.EmbeddingJobByID(ctx, jobID)
				require.NoError(t, err)
				require.Equal(t, "completed", job.State, "all-unmatched attempts finish processing")
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

func TestFaceMissingPreviewDoesNotBlockOtherPhotos(t *testing.T) {
	t.Parallel()
	x := newFaceEmbeddingFixture(t, false)
	ctx := t.Context()
	version := x.f.addPhoto(t, "healthy.jpg", 720)
	_, _, err := x.f.catalog.CreatePhotoFace(ctx, version, uuid.New().String(), document.FaceBox{Width: .2, Height: .2}, "")
	require.NoError(t, err)
	x.enable(t)
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

func TestFaceRankingContinuationWithUnavailableVectors(t *testing.T) {
	t.Parallel()
	x := newFaceEmbeddingFixture(t, false)
	ctx := t.Context()
	_, err := x.f.catalog.CreatePerson(ctx, "Ada", "operator")
	require.NoError(t, err)
	x.enable(t)
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
	x.enable(t)
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
	x.enable(t)
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

// Count payload opens at the real blob boundary without replacing ranking.
type facePayloadReads struct {
	store.RenditionBlobReader

	opens int
}

func (r *facePayloadReads) OpenStreamContext(ctx context.Context, hash string) (packstore.VerifiedReadCloser, int64, error) {
	r.opens++
	return r.RenditionBlobReader.OpenStreamContext(ctx, hash)
}

func TestFacePickerReadsOneSharedPhoto(t *testing.T) {
	t.Parallel()
	x := newFaceEmbeddingFixture(t, false)
	x.matchAll = true
	ctx := t.Context()
	named := x.inputs[0]
	_, err := x.f.catalog.DecidePhotoFace(ctx, named.ContentVersionID, store.PhotoFaceDecisionRequest{FaceID: named.FaceID, Revision: named.Revision, Action: "name", PersonName: "Ada"})
	require.NoError(t, err)
	x.enable(t)
	_, err = x.worker.ScanOnce(ctx)
	require.NoError(t, err)
	reader := &facePayloadReads{RenditionBlobReader: x.f.blobs}
	options := store.PeopleSearchOptions{FaceID: x.inputs[1].FaceID, FaceReader: reader, RecentFirst: true}
	for _, query := range []string{"", "A", "Ad", "Ada"} {
		reader.opens = 0
		people, _, err := x.f.catalog.PeopleByDisplayName(ctx, query, "", "", 100, false, options)
		require.NoError(t, err)
		require.Len(t, people, 1)
		require.Equal(t, "Ada", people[0].DisplayName)
		require.Equal(t, 1, reader.opens, "each query opens the shared photo only once")
	}
}

func newNamedFacePickerFixture(t *testing.T) (*faceEmbeddingFixture, string) {
	t.Helper()
	ctx := t.Context()
	x := newFaceEmbeddingFixture(t, false)
	x.matchAll = true
	person, err := x.f.catalog.CreatePerson(ctx, "Ada", "operator")
	require.NoError(t, err)
	personID := person.PersonID
	zoe, err := x.f.catalog.CreatePerson(ctx, "Zoe", "operator")
	require.NoError(t, err)
	for i, id := range []string{personID, personID, zoe.PersonID} {
		named := x.f.addPhoto(t, fmt.Sprintf("named-%d.jpg", i), 701+i)
		_, _, err = x.f.catalog.CreatePhotoFace(ctx, named, uuid.New().String(), document.FaceBox{X: float64(i) / 10, Width: .2, Height: .2}, id)
		require.NoError(t, err)
	}
	unrelated := x.f.addPhoto(t, "unnamed.jpg", 704)
	_, _, err = x.f.catalog.CreatePhotoFace(ctx, unrelated, uuid.New().String(), document.FaceBox{Width: .2, Height: .2}, "")
	require.NoError(t, err)
	return x, personID
}

func TestFacePickerPayloadScopeAndIndexFreshness(t *testing.T) {
	t.Parallel()
	x, personID := newNamedFacePickerFixture(t)
	ctx := t.Context()
	x.enable(t)
	count, err := x.worker.ScanOnce(ctx)
	require.NoError(t, err)
	require.Equal(t, 5, count)
	reader := &facePayloadReads{RenditionBlobReader: x.f.blobs}
	options := store.PeopleSearchOptions{FaceID: x.inputs[0].FaceID, FaceReader: reader, RecentFirst: true}
	var people []store.PersonSearchResult
	t.Run("payload scope", func(t *testing.T) {
		people, _, err = x.f.catalog.PeopleByDisplayName(ctx, "", "", "", 24, false, options)
		require.NoError(t, err)
		require.Len(t, people, 2)
		require.Equal(t, 4, reader.opens)
		reader.opens = 0
		matches, _, err := x.f.catalog.PeopleByDisplayName(ctx, "Ada", "", "", 24, false, options)
		require.NoError(t, err)
		require.Len(t, matches, 1)
		require.Equal(t, personID, matches[0].PersonID)
		require.Contains(t, matches[0].RecentRank, "#")
		require.Equal(t, 3, reader.opens, "read the source and both Ada photos, leaving Zoe's separate photo unopened")
		for _, person := range people {
			if person.PersonID == personID {
				continuation := options
				continuation.AfterRecent = person.RecentRank
				_, _, err := x.f.catalog.PeopleByDisplayName(ctx, "Ada", person.DisplayNameFolded, person.PersonID, 24, false, continuation)
				require.ErrorIs(t, err, store.ErrStaleRevision, "a different result population needs its own continuation")
			}
		}
	})
	t.Run("indexed exactness", func(t *testing.T) {
		index, err := vectorworker.NewIndexWorker(vectorworker.IndexWorkerConfig{
			Catalog: x.f.catalog, Mutate: newTestOperationGate().MutateContext, Owner: "face-picker-index",
			BuildLease: time.Minute, ReaderLease: time.Minute, IdleDelay: time.Millisecond,
			ReadVectorSet: func(ctx context.Context, member store.VectorIndexMember) ([]byte, error) {
				return x.f.catalog.ReadVectorIndexVectorSet(ctx, x.f.blobs, member)
			},
		})
		require.NoError(t, err)
		spaces, err := x.f.catalog.ListVectorIndexSpaces(ctx)
		require.NoError(t, err)
		_, err = index.Rebuild(ctx, spaces[0])
		require.NoError(t, err)
		reader.opens = 0
		indexed, _, err := x.f.catalog.PeopleByDisplayName(ctx, "", "", "", 24, false, options)
		require.NoError(t, err)
		require.Equal(t, people, indexed, "the index preserves exact ordering and continuation")
		require.Zero(t, reader.opens, "all current rows are in the built index")
	})
	t.Run("late named freshness", func(t *testing.T) {
		late := x.f.addPhoto(t, "late-named.jpg", 705)
		lateFace, _, err := x.f.catalog.CreatePhotoFace(ctx, late, uuid.New().String(), document.FaceBox{Y: .8, Width: .2, Height: .2}, "")
		require.NoError(t, err)
		x.worker.reconcileAt = time.Time{}
		_, err = x.worker.ScanOnce(ctx)
		require.NoError(t, err)
		newPerson, err := x.f.catalog.CreatePerson(ctx, "Aaron", "operator")
		require.NoError(t, err)
		_, err = x.f.catalog.DecidePhotoFace(ctx, late, store.PhotoFaceDecisionRequest{Action: "name", FaceID: lateFace.FaceID, Revision: 1, PersonID: newPerson.PersonID})
		require.NoError(t, err)
		reader.opens = 0
		current, _, err := x.f.catalog.PeopleByDisplayName(ctx, "", "", "", 24, false, options)
		require.NoError(t, err)
		require.Equal(t, newPerson.PersonID, current[0].PersonID, "the unindexed named face contributes immediately")
		require.Equal(t, 1, reader.opens, "only the photo missing from the index reads its file")
	})
}

func TestFacePickerUsesNamedFacesFromQueryPhoto(t *testing.T) {
	t.Parallel()
	x, personID := newNamedFacePickerFixture(t)
	ctx := t.Context()
	_, err := x.f.catalog.DecidePhotoFace(ctx, x.inputs[1].ContentVersionID, store.PhotoFaceDecisionRequest{Action: "name", FaceID: x.inputs[1].FaceID, Revision: 1, PersonID: personID})
	require.NoError(t, err)
	x.vectorForBox = func(box embedclient.FaceBox) []float32 {
		values := make([]float32, 512)
		if box.Y > 0 {
			values[0] = 1
		} else if box.X > .15 {
			values[0], values[1] = .28, .96
		} else {
			values[1] = 1
		}
		return values
	}
	x.enable(t)
	_, err = x.worker.ScanOnce(ctx)
	require.NoError(t, err)
	reader := &facePayloadReads{RenditionBlobReader: x.f.blobs}
	options := store.PeopleSearchOptions{FaceID: x.inputs[0].FaceID, FaceReader: reader, RecentFirst: true}
	people, _, err := x.f.catalog.PeopleByDisplayName(ctx, "", "", "", 24, true, options)
	require.NoError(t, err)
	require.Len(t, people, 2)
	require.Equal(t, personID, people[0].PersonID, "the matching named face in the query photo ranks Ada ahead of Zoe")
	require.Contains(t, people[0].RecentRank, "#", "the named source contributes to vector ranking")
	require.Equal(t, 4, reader.opens)
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
	x.enable(t)
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
	x.enable(t)
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

func TestFaceSetupDisclosesScopeAndRequiresCurrentConsent(t *testing.T) {
	t.Parallel()
	x := newFaceEmbeddingFixture(t, false)
	setup := FaceSetup{Service: x.service, Startup: func(context.Context) error { return nil }}
	reviewed, err := setup.Plan(t.Context())
	require.NoError(t, err)
	require.Contains(t, reviewed.Scope, "throughout this vault")
	require.Contains(t, reviewed.Scope, "Hidden")
	n, err := x.worker.ScanOnce(t.Context())
	require.NoError(t, err)
	require.Zero(t, n)
	require.Zero(t, x.calls)
	_, err = setup.Apply(t.Context(), "stale")
	require.ErrorIs(t, err, ErrPlanChanged)
	x.worker.reconcileAt = time.Time{}
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

// Exercise the consumer read after reopening independently of writer triggers.
// The processing fixture publishes real face generations and payloads for all three readers.
func TestFaceMatchingInputsPreserveStoredAuthority(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, damage string
	}{
		{"trashed photo", ""},
		{"missing preview", `DELETE FROM visual_preview_heads; DELETE FROM visual_preview_generations`},
		{"changed box", `UPDATE photo_faces SET width=width+0.01`},
		{"replaced preview", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			x := newFaceEmbeddingFixture(t, false)
			ctx := t.Context()
			x.enable(t)
			_, err := x.worker.ScanOnce(ctx)
			require.NoError(t, err)
			before, err := x.f.catalog.FaceMatchingInputs(ctx)
			require.NoError(t, err)
			require.Len(t, before.Sets, 1)
			set := before.Sets[0].Set
			head := store.EmbeddingHeadRecord{
				FencingToken: 2, Key: store.EmbeddingHeadKey{ContentVersionID: set.ContentVersionID, BindingID: set.BindingID, InputKind: set.InputKind},
				SetID: set.ID, VectorSpaceID: set.VectorSpace.ID, ProcessingProfileFingerprint: set.ProcessingProfileFingerprint, PublishedAt: set.CreatedAt,
			}
			require.NoError(t, x.f.catalog.PublishEmbeddingHead(ctx, head), "the publication token is valid before the source changes")
			db, err := store.DefaultSQLiteDriver().Open(x.f.databasePath, docsqlite.OpenOptions{Access: docsqlite.ReadWriteExisting, TransactionMode: docsqlite.Immediate})
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, db.Close()) })
			switch tc.name {
			case "trashed photo":
				_, _, err = x.f.catalog.Trash(ctx, x.f.node.ID, store.UnconditionalRev)
				require.NoError(t, err)
			case "replaced preview":
				// Regenerate the purged preview while retaining the earlier face vectors.
				_, err = db.ExecContext(ctx, `DELETE FROM visual_preview_heads; DELETE FROM visual_preview_generations`)
				require.NoError(t, err)
				written, err := x.f.blobs.WriteDetailedContext(ctx, bytes.NewReader(mediatest.JPEG(8, 8, color.Black)))
				require.NoError(t, err)
				canonical, _, err := document.MarshalVisualPreviewV1(document.VisualPreviewV1{
					ContractVersion: document.VisualPreviewContractV1, SourceSHA256: x.f.written.Hash,
					Recipe: x.f.recipe, State: document.VisualPreviewReady,
					Output: &document.VisualPreviewOutputV1{BlobSHA256: written.Hash, Size: written.Size, MediaType: "image/jpeg", Width: 8, Height: 8},
				})
				require.NoError(t, err)
				_, err = x.f.catalog.PublishVisualPreviewGeneration(ctx, x.f.node.CurrentVersionID, canonical, new(processingBlobPhysical(t, written)))
				require.NoError(t, err)
			default:
				for statement := range strings.SplitSeq(tc.damage, ";") {
					_, err = db.ExecContext(ctx, statement)
					require.NoError(t, err)
				}
			}
			reopened, err := store.Open(ctx, x.f.databasePath)
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, reopened.Close()) })
			after, err := reopened.FaceMatchingInputs(ctx)
			require.NoError(t, err)
			assert.Empty(t, after.Sets, "fenced or missing authority must stop contributing")
			require.ErrorIs(t, reopened.ReplaceFaceSuggestions(ctx, before, nil, false), store.ErrStaleRevision, "the captured suggestion pass must fence the same changed authority")
			err = reopened.PublishEmbeddingHead(ctx, head)
			require.ErrorIs(t, err, store.ErrEmbeddingAuthorityStale, "publication must fence the same stale face or preview evidence")
		})
	}
}

// A catalog retained by a newer binary can contain authority this reader does not support.
func TestFaceMatchingReadersRejectFutureContracts(t *testing.T) {
	t.Parallel()
	for _, table := range []string{"embedding_vector_sets", "embedding_vector_spaces"} {
		t.Run(table, func(t *testing.T) {
			t.Parallel()
			x := newFaceEmbeddingFixture(t, false)
			ctx := t.Context()
			x.enable(t)
			_, err := x.worker.ScanOnce(ctx)
			require.NoError(t, err)
			db, err := store.DefaultSQLiteDriver().Open(x.f.databasePath, docsqlite.OpenOptions{Access: docsqlite.ReadWriteExisting, TransactionMode: docsqlite.Immediate})
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, db.Close()) })
			// Model a downgrade fixture without allowing immutable authority writes in production.
			_, err = db.ExecContext(ctx, "DROP TRIGGER "+table+"_immutable_update")
			require.NoError(t, err)
			_, err = db.ExecContext(ctx, "UPDATE "+table+" SET contract_version='review-future/v99'")
			require.NoError(t, err)
			reopened, err := store.Open(ctx, x.f.databasePath)
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, reopened.Close()) })
			contractError := "embedding vector-set contract version is unsupported"
			if table == "embedding_vector_spaces" {
				contractError = "embedding vector-space contract version is unsupported"
			}
			_, err = reopened.FaceMatchingInputs(ctx)
			require.ErrorContains(t, err, contractError)
			_, _, err = reopened.PeopleByDisplayName(ctx, "", "", "", 24, false, store.PeopleSearchOptions{FaceID: x.inputs[0].FaceID, FaceReader: x.f.blobs, RecentFirst: true})
			require.ErrorContains(t, err, contractError)
			require.ErrorContains(t, x.service.rebuildFaceSuggestions(ctx), contractError)
		})
	}
}

func TestFaceRankingContinuationRejectsChangedCentroids(t *testing.T) {
	t.Parallel()
	x := newFaceEmbeddingFixture(t, false)
	x.matchAll = true
	ctx := t.Context()
	ada, err := x.f.catalog.CreatePerson(ctx, "Ada", "operator")
	require.NoError(t, err)
	beth, err := x.f.catalog.CreatePerson(ctx, "Beth", "operator")
	require.NoError(t, err)
	cara, err := x.f.catalog.CreatePerson(ctx, "Cara", "operator")
	require.NoError(t, err)
	version := x.f.addPhoto(t, "ranking-evidence.jpg", 720)
	var moving store.PhotoFace
	for i, person := range []string{ada.PersonID, ada.PersonID, beth.PersonID, cara.PersonID} {
		face, _, err := x.f.catalog.CreatePhotoFace(ctx, version, uuid.New().String(), document.FaceBox{X: .1 + float64(i)*.2, Y: .6, Width: .1, Height: .1}, person)
		require.NoError(t, err)
		if i == 0 {
			moving = face
		}
	}
	x.vectorForBox = func(box embedclient.FaceBox) []float32 {
		angle := 0.0
		if box.Y > .5 {
			switch {
			case box.X < .2:
				angle = 0
			case box.X < .4:
				angle = 2.4
			case box.X < .6:
				angle = 1.7
			default:
				angle = 2.8
			}
		}
		values := make([]float32, 512)
		values[0], values[1] = float32(math.Cos(angle)), float32(math.Sin(angle))
		return values
	}
	x.enable(t)
	_, err = x.worker.ScanOnce(ctx)
	require.NoError(t, err)
	options := store.PeopleSearchOptions{FaceID: x.inputs[0].FaceID, FaceReader: x.f.blobs, RecentFirst: true}
	first, _, err := x.f.catalog.PeopleByDisplayName(ctx, "", "", "", 2, false, options)
	require.NoError(t, err)
	require.Len(t, first, 2)
	require.Equal(t, "Ada", first[0].DisplayName)
	require.Equal(t, "Beth", first[1].DisplayName)
	_, err = x.f.catalog.DecidePhotoFace(ctx, version, store.PhotoFaceDecisionRequest{FaceID: moving.FaceID, Revision: moving.Revision, Action: "name", PersonID: cara.PersonID})
	require.NoError(t, err)
	fresh, _, err := x.f.catalog.PeopleByDisplayName(ctx, "", "", "", 24, false, options)
	require.NoError(t, err)
	require.Len(t, fresh, 3)
	require.Equal(t, "Cara", fresh[0].DisplayName)
	require.Equal(t, "Beth", fresh[1].DisplayName)
	require.Equal(t, "Ada", fresh[2].DisplayName)
	options.AfterRecent = first[1].RecentRank
	_, _, err = x.f.catalog.PeopleByDisplayName(ctx, "", first[1].DisplayNameFolded, first[1].PersonID, 2, false, options)
	require.ErrorIs(t, err, store.ErrStaleRevision, "changed centroids must invalidate continuation instead of repeating Ada and omitting Cara")
}
