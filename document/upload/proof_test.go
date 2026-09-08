package upload

import (
	"bytes"
	jsonv1 "encoding/json"
	jsonv2 "encoding/json/v2"
	"fmt"
	"io"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/document/media"
)

func TestAuthorizeIssuesProofFromReinspectedSpool(t *testing.T) {
	data := []byte("synthetic proof text\n")
	record := inspectCapability(t, data)
	authorized, err := Authorize(t.Context(), Source{
		Reader: io.NopCloser(bytes.NewReader(data)), Directory: t.TempDir(),
	}, record, UploadMetadata{Filename: "notes.txt"})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, authorized.Close()) })

	carrier, ok := authorized.(document.VerifiedUploadProofCarrier)
	require.True(t, ok)
	proof, present := carrier.VerifiedUploadProof()
	require.True(t, present)
	require.True(t, proof.Valid())
	assert.Equal(t, document.VerifiedUploadFacts{
		SourceBytes: int64(len(data)), SourceSHA256: record.SourceSHA256,
		CapabilityRecordChecksum: record.Checksum,
		DescriptorFingerprint:    record.DescriptorFingerprint,
		ProfileFingerprint:       record.ProfileFingerprint, DisclosureFingerprint: record.DisclosureFingerprint,
		InputKind: string(record.InputKind), MediaFamily: record.MediaFamily,
		MediaType: record.MediaType, Format: record.Format,
		Pages: record.Measurements.Pages, Pixels: record.Measurements.Pixels,
		Frames: record.Measurements.Frames, DurationMS: record.Measurements.DurationMS,
		MaxSourceBytes: record.Policy.MaxSourceBytes, MaxPages: record.Policy.MaxPages,
		MaxPixels: record.Policy.MaxPixels, MaxFrames: record.Policy.MaxFrames,
		MaxDurationMS: record.Policy.MaxDurationMS,
	}, proof.Snapshot())
	assert.NotContains(t, fmt.Sprintf("%+v", proof), "notes.txt")
	_, err = jsonv1.Marshal(proof)
	require.Error(t, err)
	_, exposesRecord := authorized.(interface{ CapabilityRecord() media.CapabilityRecord })
	assert.False(t, exposesRecord)
}

func TestAuthorizedUploadNilReceiverHasNoProof(t *testing.T) {
	var authorized *authorizedUpload
	proof, present := authorized.VerifiedUploadProof()
	assert.False(t, present)
	assert.False(t, proof.Valid())
}

// This test fails if Authorize treats a checksum-valid, locally authoritative
// record as proof of measured facts instead of comparing it with independent
// spool reinspection.
func TestAuthorizeRejectsChecksumValidResealedMeasurementMutation(t *testing.T) {
	data := []byte("synthetic proof text\n")
	record := inspectCapability(t, data)

	t.Run("unchanged record issues proof", func(t *testing.T) {
		directory := t.TempDir()
		reader := &countingReadCloser{Reader: bytes.NewReader(data)}
		authorized, err := Authorize(t.Context(), Source{
			Reader: reader, Directory: directory,
		}, record, UploadMetadata{Filename: "notes.txt"})
		require.NoError(t, err)
		carrier, ok := authorized.(document.VerifiedUploadProofCarrier)
		require.True(t, ok)
		proof, present := carrier.VerifiedUploadProof()
		require.True(t, present)
		require.True(t, proof.Valid())
		require.NoError(t, authorized.Close())
		assert.True(t, reader.didClose)
		assert.Empty(t, spoolEntries(t, directory))
	})

	t.Run("resealed measurement is rejected after reinspection", func(t *testing.T) {
		mutated := record
		mutated.Measurements.TextLines++
		mutated.Checksum = ""
		encoded, err := jsonv2.Marshal(capabilityRecordChecksumIdentity(mutated), jsonv2.Deterministic(true))
		require.NoError(t, err)
		mutated.Checksum = sha256Hex(encoded)
		require.NoError(t, media.ValidateCapabilityRecord(mutated),
			"the test mutation must cross the public checksum-validation boundary")

		directory := t.TempDir()
		reader := &countingReadCloser{Reader: bytes.NewReader(data)}
		authorized, err := Authorize(t.Context(), Source{
			Reader: reader, Directory: directory,
		}, mutated, UploadMetadata{Filename: "notes.txt"})
		require.ErrorContains(t, err, "sealed spool capability does not match authorization")
		assert.Nil(t, authorized)
		assert.True(t, reader.didClose)
		assert.Empty(t, spoolEntries(t, directory))
	})
}

// capabilityRecordChecksumIdentity mirrors the production record's public
// canonical JSON identity without inheriting its JSON methods.
type capabilityRecordChecksumIdentity media.CapabilityRecord
