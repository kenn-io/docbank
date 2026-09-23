package store

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"testing"
	"time"
	"uuid"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/internal/canonical"
)

func TestPackageRecordKeysSeparateStructuredInputs(t *testing.T) {
	first, err := PackageRecordKey("a", 1, "x/2/y")
	require.NoError(t, err)
	second, err := PackageRecordKey("a/1/x", 2, "y")
	require.NoError(t, err)
	require.NotEqual(t, first, second)
	require.True(t, canonical.IsSHA256Hex(first))
	require.Len(t, PackageOccurrenceID("package-a", first), 32)
	require.NotEqual(t, PackageOccurrenceID("package-a", first), PackageOccurrenceID("package-b", first))
	_, err = PackageRecordKey("a", 0, "x")
	require.ErrorIs(t, err, ErrPackageConflict)
}

func prepareReceivedPackage(t *testing.T, s *Store, name string) (PackageRequest, IngestRun, Node) {
	t.Helper()
	request := validPackageRequest(t, s)
	run, err := s.BeginIngest(t.Context(), "package:loadfile", name)
	require.NoError(t, err)
	content := []byte("Synthetic imported record " + name)
	digest := sha256.Sum256(content)
	node, err := s.IngestFileExact(t.Context(), run, s.RootID(), name+".txt",
		hex.EncodeToString(digest[:]), int64(len(content)), "text/plain", name+".txt", "")
	require.NoError(t, err)
	request.SnapshotID = ""
	request.Direction = packageDirectionReceived
	request.State = "importing"
	request.IngestID = run.ID()
	request.ManifestSHA256 = request.ManifestBlobSHA256
	return request, run, node
}

func packageImportJobRequest(t *testing.T, s *Store, pkg PackageRequest) PackageImportJobRequest {
	t.Helper()
	preflightID := uuid.New().String()
	owner := "synthetic-operator"
	_, err := s.PutPackagePreflight(t.Context(), PackagePreflightRecord{
		PreflightID: preflightID, Owner: owner, SourceKind: "root", SourceRef: "synthetic-root-digest",
		SourceLocator: "synthetic-root", ProfileJSON: pkg.ProfileJSON, MappingJSON: pkg.MappingJSON,
		ProfileSHA256: pkg.ProfileSHA256, MappingSHA256: pkg.MappingSHA256,
		ManifestSHA256: pkg.ManifestSHA256, ManifestBlobSHA256: pkg.ManifestBlobSHA256,
		CanonicalJSON: []byte(`{}`), DiagnosticsJSON: []byte(`[]`),
	})
	require.NoError(t, err)
	return PackageImportJobRequest{ID: uuid.New().String(), Owner: owner, OperationID: uuid.New().String(),
		RequestSHA256: fakeHash("ab"), PreflightID: preflightID, PackageID: pkg.PackageID,
		JobJSON: []byte(`{"accept_partial":false,"index_supplied_text":true,"source_kind":"root","source_locator":"synthetic-root"}`)}
}

func seedReceivedPackage(t *testing.T, s *Store, name string) (Package, Node) {
	t.Helper()
	request, run, node := prepareReceivedPackage(t, s, name)
	_, err := s.AdmitPackageImport(t.Context(), run, request, packageImportJobRequest(t, s, request))
	require.NoError(t, err)
	pkg, err := s.Package(t.Context(), request.PackageID)
	require.NoError(t, err)
	return pkg, node
}

func TestCommitPackageRecordWithLeaseReplaysExactPayload(t *testing.T) {
	s := newTestStore(t)
	pkg, node := seedReceivedPackage(t, s, "alpha")
	job, err := s.ClaimPackageImportJob(t.Context(), "record-worker", time.Minute)
	require.NoError(t, err)
	key, err := PackageRecordKey("VOL001.dat", 1, "DOC-A")
	require.NoError(t, err)
	occurrence := PackageOccurrenceID(pkg.PackageID, key)
	raw := []byte(`{"BEGBATES":"EXT000001"}`)
	hash := sha256.Sum256(raw)
	record := PackageRecordRow{PackageID: pkg.PackageID, RowID: key, LoadFile: "VOL001.dat",
		RowOrdinal: 1, OccurrenceID: occurrence, RawJSON: raw,
		RawSHA256: hex.EncodeToString(hash[:]), Sensitive: true}
	labels := []PackageLabelRow{{PackageID: pkg.PackageID, Provenance: packageDirectionReceived, LabelSet: "EXT",
		Label: "EXT000001", LabelSortKey: LabelSortKey("EXT000001"), OccurrenceID: occurrence,
		ContentVersionID: node.CurrentVersionID, PageState: "unknown", Endpoint: "begin"}}
	for _, number := range []int{2, 1} {
		page := labels[0]
		page.Endpoint, page.PageNumber = "page", number
		labels = append(labels, page)
	}
	receiptID, err := newUUIDv4()
	require.NoError(t, err)
	receipt := PackageImportReceipt{ReceiptID: receiptID, PackageID: pkg.PackageID,
		RecordKey: key, OccurrenceID: occurrence, ContentVersionID: node.CurrentVersionID,
		State: "committed", ReceiptJSON: []byte(`{}`)}
	first, err := s.CommitPackageRecordWithLease(t.Context(), job.ID, job.Epoch, job.Token, record, labels, receipt)
	require.NoError(t, err)
	second, err := s.CommitPackageRecordWithLease(t.Context(), job.ID, job.Epoch, job.Token, record, labels, receipt)
	require.NoError(t, err)
	require.Equal(t, first, second)
	_, err = s.db.ExecContext(t.Context(), `INSERT INTO package_labels SELECT * FROM package_labels
		WHERE package_id=? AND endpoint='begin'`, pkg.PackageID)
	require.Error(t, err, "document labels must retain a unique identity without a page number")
	_, err = s.db.ExecContext(t.Context(), `INSERT INTO package_labels(
		package_id,provenance,label_set,label,label_sort_key,occurrence_id,content_version_id,
		page_state,endpoint) VALUES(?,?,?,?,?,?,?,?,?)`, pkg.PackageID, "assigned", "LOCAL",
		"OUR000041", "OUR000041", occurrence, node.CurrentVersionID, "unknown", "begin")
	require.NoError(t, err)
	third, err := s.CommitPackageRecordWithLease(t.Context(), job.ID, job.Epoch, job.Token, record, labels, receipt)
	require.NoError(t, err, "an assigned label cannot change a received-record replay")
	require.Equal(t, first, third)
	_, err = s.db.ExecContext(t.Context(), `UPDATE packages SET state='complete',completed_at=? WHERE package_id=?`,
		nowRFC3339(), pkg.PackageID)
	require.NoError(t, err)
	afterCompletion, err := s.CommitPackageRecordWithLease(t.Context(), job.ID, job.Epoch, job.Token, record, labels, receipt)
	require.NoError(t, err)
	require.Equal(t, first, afterCompletion)
	read, err := s.PackageRecord(t.Context(), pkg.PackageID, key)
	require.NoError(t, err)
	require.Equal(t, record, read)
	changed := record
	changed.RawJSON = []byte(`{"BEGBATES":"EXT000009"}`)
	changedHash := sha256.Sum256(changed.RawJSON)
	changed.RawSHA256 = hex.EncodeToString(changedHash[:])
	_, err = s.CommitPackageRecordWithLease(t.Context(), job.ID, job.Epoch, job.Token, changed, labels, receipt)
	require.ErrorIs(t, err, ErrPackageConflict)
	changedLabels := append([]PackageLabelRow{}, labels...)
	changedLabels[0].Label = "EXT000009"
	changedLabels[0].LabelSortKey = LabelSortKey("EXT000009")
	_, err = s.CommitPackageRecordWithLease(t.Context(), job.ID, job.Epoch, job.Token, record, changedLabels, receipt)
	require.ErrorIs(t, err, ErrPackageConflict)
}

func TestAssignPackageLabelsIsIdempotentAndSeparateFromReceivedAuthority(t *testing.T) {
	s := newTestStore(t)
	pkg, node := seedReceivedPackage(t, s, "assigned-labels")
	key, err := PackageRecordKey("VOL001.dat", 1, "DOC-A")
	require.NoError(t, err)
	occurrence := PackageOccurrenceID(pkg.PackageID, key)
	commitReceivedLabel(t, s, pkg, node.CurrentVersionID, "EXT000001")
	received := PackageLabelRow{PackageID: pkg.PackageID, Provenance: packageDirectionReceived,
		LabelSet: "sender", Label: "EXT000001", LabelSortKey: "EXT000001", OccurrenceID: occurrence,
		ContentVersionID: node.CurrentVersionID, PageState: "unknown", Endpoint: "begin"}
	assigned := PackageLabelRow{PackageID: pkg.PackageID, Provenance: "assigned", LabelSet: "CASE",
		Label: "CASE000001", LabelSortKey: "CASE000001", OccurrenceID: occurrence,
		ContentVersionID: node.CurrentVersionID, PageState: "verified", Endpoint: "begin"}
	pageOne, pageTwo := assigned, assigned
	pageOne.Endpoint, pageOne.PageNumber = "page", 1
	pageTwo.Endpoint, pageTwo.PageNumber = "page", 2
	require.NoError(t, s.AssignPackageLabels(t.Context(), pkg.PackageID, occurrence,
		node.CurrentVersionID, []PackageLabelRow{pageTwo, assigned, pageOne}))
	require.NoError(t, s.AssignPackageLabels(t.Context(), pkg.PackageID, occurrence,
		node.CurrentVersionID, []PackageLabelRow{pageOne, pageTwo, assigned}))
	labels, err := s.PackageLabels(t.Context(), pkg.PackageID, occurrence)
	require.NoError(t, err)
	require.Equal(t, []PackageLabelRow{assigned, pageOne, pageTwo, received}, labels)
	var original bytes.Buffer
	require.NoError(t, s.ExportMetadata(t.Context(), &original))
	restored := newTestStore(t)
	require.NoError(t, restored.ImportMetadata(t.Context(), bytes.NewReader(original.Bytes())))
	require.NoError(t, restored.AssignPackageLabels(t.Context(), pkg.PackageID, occurrence,
		node.CurrentVersionID, []PackageLabelRow{pageTwo, pageOne, assigned}))
	restoredLabels, err := restored.PackageLabels(t.Context(), pkg.PackageID, occurrence)
	require.NoError(t, err)
	require.Equal(t, labels, restoredLabels)
	var exported bytes.Buffer
	require.NoError(t, restored.ExportMetadata(t.Context(), &exported))
	require.Equal(t, original.Bytes(), exported.Bytes())
	changed := assigned
	changed.Label = "CASE000002"
	changed.LabelSortKey = changed.Label
	require.ErrorIs(t, s.AssignPackageLabels(t.Context(), pkg.PackageID, occurrence,
		node.CurrentVersionID, []PackageLabelRow{changed}), ErrPackageConflict)
}

func commitReceivedLabel(t *testing.T, s *Store, pkg Package, versionID, label string) {
	t.Helper()
	job, err := s.ClaimPackageImportJob(t.Context(), "record-worker", time.Minute)
	require.NoError(t, err)
	require.Equal(t, pkg.PackageID, job.PackageID)
	key, err := PackageRecordKey("VOL001.dat", 1, "DOC-A")
	require.NoError(t, err)
	occurrence := PackageOccurrenceID(pkg.PackageID, key)
	raw := []byte(`{"BEGBATES":"` + label + `"}`)
	hash := sha256.Sum256(raw)
	record := PackageRecordRow{PackageID: pkg.PackageID, RowID: key, LoadFile: "VOL001.dat",
		RowOrdinal: 1, OccurrenceID: occurrence, RawJSON: raw,
		RawSHA256: hex.EncodeToString(hash[:])}
	labelRow := PackageLabelRow{PackageID: pkg.PackageID, Provenance: packageDirectionReceived,
		LabelSet: "sender", Label: label, LabelSortKey: LabelSortKey(label),
		OccurrenceID: occurrence, ContentVersionID: versionID,
		PageState: "unknown", Endpoint: "begin"}
	receiptID, err := newUUIDv4()
	require.NoError(t, err)
	_, err = s.CommitPackageRecordWithLease(t.Context(), job.ID, job.Epoch, job.Token, record, []PackageLabelRow{labelRow}, PackageImportReceipt{
		ReceiptID: receiptID, PackageID: pkg.PackageID, RecordKey: key,
		OccurrenceID: occurrence, ContentVersionID: versionID, State: "committed",
		ReceiptJSON: []byte(`{}`),
	})
	require.NoError(t, err)
	version, err := s.ContentVersionByID(t.Context(), versionID)
	require.NoError(t, err)
	members := streamSnapshotMembers(t, CollectionSnapshotMember{Ordinal: 1, OccurrenceID: occurrence, NodeID: version.NodeID,
		ContentVersionID: versionID, BlobSHA256: version.BlobHash, Size: version.Size,
		FamilyID: occurrence, FamilyOrder: 1, DocumentKind: "other", FrozenFieldsJSON: "{}"})
	_, err = s.FinishPackageImportJob(t.Context(), job.ID, job.Epoch, job.Token, "complete", uuid.New().String(), members)
	require.NoError(t, err)
}

func TestReceivedLabelLookupRequiresScopeWhenSenderReusesIt(t *testing.T) {
	s := newTestStore(t)
	first, firstNode := seedReceivedPackage(t, s, "alpha")
	commitReceivedLabel(t, s, first, firstNode.CurrentVersionID, "EXT000001")
	run, err := s.BeginIngest(t.Context(), "package:loadfile", "beta")
	require.NoError(t, err)
	content := []byte("Second imported record")
	digest := sha256.Sum256(content)
	secondNode, err := s.IngestFileExact(t.Context(), run, s.RootID(), "beta.txt",
		hex.EncodeToString(digest[:]), int64(len(content)), "text/plain", "beta.txt", "")
	require.NoError(t, err)
	secondRequest := first.PackageRequest
	secondRequest.PackageID, err = newUUIDv4()
	require.NoError(t, err)
	secondRequest.IngestID = run.ID()
	_, err = s.AdmitPackageImport(t.Context(), run, secondRequest, packageImportJobRequest(t, s, secondRequest))
	require.NoError(t, err)
	second, err := s.Package(t.Context(), secondRequest.PackageID)
	require.NoError(t, err)
	commitReceivedLabel(t, s, second, secondNode.CurrentVersionID, "EXT000001")
	_, err = s.LookupPackageLabel(t.Context(), "EXT000001", "", "", packageDirectionReceived)
	require.ErrorIs(t, err, ErrAmbiguousLabel)
	require.ErrorContains(t, err, first.PackageID)
	require.ErrorContains(t, err, second.PackageID)
	rows, err := s.LookupPackageLabel(t.Context(), "EXT000001", first.PackageID, "", packageDirectionReceived)
	require.NoError(t, err)
	require.Len(t, rows, 1)
	require.Equal(t, first.PackageID, rows[0].PackageID)
	require.Equal(t, "unknown", rows[0].PageState)
}

func TestReceivedLabelLookupTreatsDocumentAndPageEndpointsAsOneOccurrence(t *testing.T) {
	s := newTestStore(t)
	pkg, node := seedReceivedPackage(t, s, "same-occurrence")
	job, err := s.ClaimPackageImportJob(t.Context(), "record-worker", time.Minute)
	require.NoError(t, err)
	key, err := PackageRecordKey("VOL001.dat", 1, "DOC-A")
	require.NoError(t, err)
	occurrence := PackageOccurrenceID(pkg.PackageID, key)
	raw := []byte(`{"BEGBATES":"EXT000001"}`)
	digest := sha256.Sum256(raw)
	base := PackageLabelRow{PackageID: pkg.PackageID, Provenance: packageDirectionReceived,
		LabelSet: "sender", Label: "EXT000001", LabelSortKey: LabelSortKey("EXT000001"),
		OccurrenceID: occurrence, ContentVersionID: node.CurrentVersionID}
	begin := base
	begin.Endpoint, begin.PageState = "begin", "unknown"
	page := base
	page.Endpoint, page.PageState, page.PageNumber = "page", "verified", 1
	receiptID, err := newUUIDv4()
	require.NoError(t, err)
	_, err = s.CommitPackageRecordWithLease(t.Context(), job.ID, job.Epoch, job.Token, PackageRecordRow{PackageID: pkg.PackageID,
		RowID: key, LoadFile: "VOL001.dat", RowOrdinal: 1, OccurrenceID: occurrence,
		RawJSON: raw, RawSHA256: hex.EncodeToString(digest[:])}, []PackageLabelRow{begin, page},
		PackageImportReceipt{ReceiptID: receiptID, PackageID: pkg.PackageID, RecordKey: key,
			OccurrenceID: occurrence, ContentVersionID: node.CurrentVersionID,
			State: "committed", ReceiptJSON: []byte(`{}`)})
	require.NoError(t, err)
	labels, err := s.LookupPackageLabel(t.Context(), "EXT000001", pkg.PackageID, "sender", packageDirectionReceived)
	require.NoError(t, err)
	require.Len(t, labels, 2)
}

func TestPackageImportReceiptsAndLabelsSurviveMetadataRestore(t *testing.T) {
	s := newTestStore(t)
	pkg, node := seedReceivedPackage(t, s, "portable")
	commitReceivedLabel(t, s, pkg, node.CurrentVersionID, "EXT000001")
	var original bytes.Buffer
	require.NoError(t, s.ExportMetadata(t.Context(), &original))
	restored := newTestStore(t)
	require.NoError(t, restored.ImportMetadata(t.Context(), bytes.NewReader(original.Bytes())))
	rows, err := restored.LookupPackageLabel(t.Context(), "EXT000001", pkg.PackageID, "", packageDirectionReceived)
	require.NoError(t, err)
	require.Len(t, rows, 1)
	require.Equal(t, node.CurrentVersionID, rows[0].ContentVersionID)
	key, err := PackageRecordKey("VOL001.dat", 1, "DOC-A")
	require.NoError(t, err)
	head, err := restored.PackageImportHead(t.Context(), pkg.PackageID, key)
	require.NoError(t, err)
	require.Equal(t, PackageOccurrenceID(pkg.PackageID, key), head.OccurrenceID)
	var replay bytes.Buffer
	require.NoError(t, restored.ExportMetadata(t.Context(), &replay))
	require.Equal(t, original.Bytes(), replay.Bytes())
}

func TestPackageRecordRejectsVersionOutsideItsIngest(t *testing.T) {
	s := newTestStore(t)
	pkg, _ := seedReceivedPackage(t, s, "owned")
	job, err := s.ClaimPackageImportJob(t.Context(), "record-worker", time.Minute)
	require.NoError(t, err)
	foreign, err := s.CreateFile(t.Context(), s.RootID(), "foreign.txt", fakeHash("fe"), 10, "text/plain")
	require.NoError(t, err)
	key, err := PackageRecordKey("VOL001.dat", 2, "DOC-B")
	require.NoError(t, err)
	occurrence := PackageOccurrenceID(pkg.PackageID, key)
	raw := []byte(`{}`)
	digest := sha256.Sum256(raw)
	record := PackageRecordRow{PackageID: pkg.PackageID, RowID: key, LoadFile: "VOL001.dat",
		RowOrdinal: 2, OccurrenceID: occurrence, RawJSON: raw, RawSHA256: hex.EncodeToString(digest[:])}
	receiptID, err := newUUIDv4()
	require.NoError(t, err)
	_, err = s.CommitPackageRecordWithLease(t.Context(), job.ID, job.Epoch, job.Token, record, nil, PackageImportReceipt{
		ReceiptID: receiptID, PackageID: pkg.PackageID, RecordKey: key,
		OccurrenceID: occurrence, ContentVersionID: foreign.CurrentVersionID,
		State: "committed", ReceiptJSON: []byte(`{}`),
	})
	require.ErrorIs(t, err, ErrPackageConflict)
}
