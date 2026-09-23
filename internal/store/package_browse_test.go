package store

import (
	"crypto/sha256"
	"encoding/hex"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/internal/canonical"
	"go.kenn.io/docbank/internal/loadfile"
)

func TestPackageMembersReadSealedOccurrenceThroughPackageID(t *testing.T) {
	s := newTestStore(t)
	request := validPackageRequest(t, s)
	run, err := s.BeginIngest(t.Context(), "package:loadfile", "synthetic")
	require.NoError(t, err)
	publishPackageTestIngest(t, s, run, "member-source.txt")
	request.Direction, request.State, request.IngestID = "received", "importing", run.ID()
	_, err = s.CreatePackage(t.Context(), request)
	require.NoError(t, err)
	members, err := s.PackageMembers(t.Context(), request.PackageID, 0, 100)
	require.NoError(t, err)
	require.Len(t, members, 1)
	require.Equal(t, 1, members[0].Ordinal)
	require.NotEmpty(t, members[0].ContentVersionID)
	empty, err := s.PackageMembers(t.Context(), request.PackageID, 1, 100)
	require.NoError(t, err)
	require.Empty(t, empty)
	_, err = s.PackageMembers(t.Context(), request.PackageID, 0, 251)
	require.ErrorIs(t, err, ErrPackageConflict)
}

func TestPackageTimelineInputsPreserveRawDateAndDeclaredZone(t *testing.T) {
	s := newTestStore(t)
	request := validPackageRequest(t, s)
	profile := loadfile.Profile{ID: "dat-concordance-v1", DeclaredTimezone: "Europe/Berlin"}
	profileJSON, err := canonical.Marshal(profile)
	require.NoError(t, err)
	profileHash := sha256.Sum256(profileJSON)
	request.ProfileJSON = string(profileJSON)
	request.ProfileSHA256 = hex.EncodeToString(profileHash[:])
	run, err := s.BeginIngest(t.Context(), "package:loadfile", "synthetic")
	require.NoError(t, err)
	imported := publishPackageTestIngest(t, s, run, "timeline-source.txt")
	request.Direction, request.State, request.IngestID = "received", "importing", run.ID()
	request.SnapshotID = ""
	request.ManifestSHA256 = request.ManifestBlobSHA256
	_, err = s.AdmitPackageImport(t.Context(), run, request, packageImportJobRequest(t, s, request))
	require.NoError(t, err)
	job, err := s.ClaimPackageImportJob(t.Context(), "timeline-worker", time.Minute)
	require.NoError(t, err)
	firstDoc, secondDoc := timelineDocIDsWithDescendingKeys(t)
	firstKey := commitTimelineInput(t, s, job, imported.CurrentVersionID, 1, firstDoc, `{"DATESENT":"03/04/2026"}`)
	commitTimelineInput(t, s, job, imported.CurrentVersionID, 2, secondDoc, `{"DATESENT":"04/04/2026"}`)
	inputs, err := s.PackageTimelineInputs(t.Context(), request.PackageID, "", 1)
	require.NoError(t, err)
	require.Len(t, inputs, 1)
	require.JSONEq(t, `{"DATESENT":"03/04/2026"}`, string(inputs[0].RawJSON))
	require.Equal(t, "Europe/Berlin", inputs[0].DeclaredTimezone)
	require.Equal(t, request.PackageID, inputs[0].PackageID)
	next, err := s.PackageTimelineInputs(t.Context(), request.PackageID, firstKey, 1)
	require.NoError(t, err)
	require.Len(t, next, 1)
	require.JSONEq(t, `{"DATESENT":"04/04/2026"}`, string(next[0].RawJSON))
	_, err = s.PackageTimelineInputs(t.Context(), request.PackageID, "", 251)
	require.ErrorIs(t, err, ErrPackageConflict)
}

func timelineDocIDsWithDescendingKeys(t *testing.T) (string, string) {
	t.Helper()
	first := "DOC-0"
	firstKey, err := PackageRecordKey("VOL001.dat", 1, first)
	require.NoError(t, err)
	for index := 1; index < 1000; index++ {
		second := "DOC-" + strconv.Itoa(index)
		secondKey, keyErr := PackageRecordKey("VOL001.dat", 2, second)
		require.NoError(t, keyErr)
		if secondKey < firstKey {
			return first, second
		}
	}
	t.Fatal("could not construct descending row IDs")
	return "", ""
}

func commitTimelineInput(t *testing.T, s *Store, job PackageImportJob, versionID string, ordinal int, docID, value string) string {
	t.Helper()
	key, err := PackageRecordKey("VOL001.dat", ordinal, docID)
	require.NoError(t, err)
	raw := []byte(value)
	rawHash := sha256.Sum256(raw)
	receiptID, err := newUUIDv4()
	require.NoError(t, err)
	occurrence := PackageOccurrenceID(job.PackageID, key)
	_, err = s.CommitPackageRecordWithLease(t.Context(), job.ID, job.Epoch, job.Token, PackageRecordRow{PackageID: job.PackageID,
		RowID: key, LoadFile: "VOL001.dat", RowOrdinal: ordinal, OccurrenceID: occurrence,
		RawJSON: raw, RawSHA256: hex.EncodeToString(rawHash[:])}, nil, PackageImportReceipt{
		ReceiptID: receiptID, PackageID: job.PackageID, RecordKey: key, OccurrenceID: occurrence,
		ContentVersionID: versionID, State: "committed", ReceiptJSON: []byte(`{}`),
	})
	require.NoError(t, err)
	return key
}

func TestPackageLabelCandidatesPageAmbiguousSenderLabels(t *testing.T) {
	s := newTestStore(t)
	first, firstNode := seedReceivedPackage(t, s, "candidate-a")
	commitReceivedLabel(t, s, first, firstNode.CurrentVersionID, "EXT000001")
	_, _, err := s.MovePath(t.Context(), "/package.txt", "/package-one.txt")
	require.NoError(t, err)
	second, secondNode := seedReceivedPackage(t, s, "candidate-b")
	commitReceivedLabel(t, s, second, secondNode.CurrentVersionID, "EXT000001")
	page, next, err := s.PackageLabelCandidates(t.Context(), "EXT000001", "", "sender", "received", "", 1)
	require.NoError(t, err)
	require.Len(t, page, 1)
	require.NotEmpty(t, next)
	last, final, err := s.PackageLabelCandidates(t.Context(), "EXT000001", "", "sender", "received", next, 1)
	require.NoError(t, err)
	require.Len(t, last, 1)
	require.Empty(t, final)
	require.NotEqual(t, page[0].PackageID, last[0].PackageID)
	scoped, next, err := s.PackageLabelCandidates(t.Context(), "EXT000001", first.PackageID, "", "received", "", 100)
	require.NoError(t, err)
	require.Len(t, scoped, 1)
	require.Empty(t, next)
	record, err := s.PackageRecordByOccurrence(t.Context(), first.PackageID, scoped[0].OccurrenceID)
	require.NoError(t, err)
	require.Equal(t, scoped[0].OccurrenceID, record.OccurrenceID)
	labels, err := s.PackageLabels(t.Context(), first.PackageID, scoped[0].OccurrenceID)
	require.NoError(t, err)
	require.Equal(t, scoped, labels)
}

func publishPackageTestIngest(t *testing.T, s *Store, run IngestRun, name string) Node {
	t.Helper()
	body := []byte("synthetic received bytes for " + name)
	digest := sha256.Sum256(body)
	node, err := s.IngestFileExact(t.Context(), run, s.RootID(), name, hex.EncodeToString(digest[:]),
		int64(len(body)), "text/plain", name, "")
	require.NoError(t, err)
	return node
}
