package store

import (
	"testing"

	"github.com/stretchr/testify/require"

	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/internal/pgtest"
	"go.kenn.io/docbank/internal/store/pgschema"
)

func checkPhotoFaceMetadataPostgres(t *testing.T) {
	t.Helper()
	t.Parallel()
	ctx := t.Context()
	setup, owner := pgtest.NewDatabase(t)
	destination := newPostgresSchemaStore(t, setup, pgschema.Config{Schema: "documents", SchemaOwner: owner})
	fixture := newFaceDetectionTestFixture(t)
	require.NoError(t, fixture.scan("b1", document.FaceObservation{
		Box:  document.FaceBox{X: .1, Y: .1, Width: .2, Height: .2},
		Roll: new(0.0), Facing: new(.9), EyesOpen: new(true), Smile: new(false),
	}))
	manualID, err := newUUIDv4()
	require.NoError(t, err)
	_, _, err = fixture.s.CreatePhotoFace(ctx, fixture.version, manualID, document.FaceBox{X: .7, Y: .7, Width: .2, Height: .2}, "")
	require.NoError(t, err)
	copyStoreCatalogFixture(t, fixture.s, setup, postgresEnrollmentCatalogs)

	var faces []metadataPhotoFace
	require.NoError(t, photoFaceMetadata.export(ctx, destination.db, func(value any) error {
		face, ok := value.(metadataPhotoFace)
		require.True(t, ok)
		faces = append(faces, face)
		return nil
	}))
	require.Len(t, faces, 2)
	for _, face := range faces {
		if face.FaceID == manualID {
			require.Nil(t, face.Roll)
			require.Nil(t, face.Facing)
			require.Nil(t, face.EyesOpen)
			require.Nil(t, face.Smile)
			require.Nil(t, face.DetectorFingerprint)
		} else {
			require.Equal(t, new(0.0), face.Roll)
			require.Equal(t, new(.9), face.Facing)
			require.Equal(t, new(true), face.EyesOpen)
			require.Equal(t, new(false), face.Smile)
			require.Equal(t, new(fakeHash("b1")), face.DetectorFingerprint)
		}
	}
	plan, err := destination.PreviewInitialAudit(ctx, destination.RootID(), "cli", nil)
	require.NoError(t, err)
	_, err = destination.EnableInitialAudit(ctx, plan)
	require.NoError(t, err)
}
