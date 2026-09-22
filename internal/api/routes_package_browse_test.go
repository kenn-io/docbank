package api_test

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json/v2"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/canonical"
	"go.kenn.io/docbank/internal/loadfile"
	"go.kenn.io/docbank/internal/store"
)

func TestPackageBrowseRoutesPreserveScopedAuthority(t *testing.T) {
	srv, catalog := newPackageTestServer(t)
	received, rowID, raw := seedBrowseReceivedPackage(t, catalog, false)
	produced := seedBrowseProducedPackage(t, catalog)

	list := srv.get(t, "/api/v1/packages?limit=1")
	require.Equal(t, http.StatusOK, list.Code, list.Body.String())
	var first api.PackagePage
	require.NoError(t, json.Unmarshal(list.Body.Bytes(), &first))
	require.Len(t, first.Items, 1)
	require.NotEmpty(t, first.NextAfter)
	second := srv.get(t, "/api/v1/packages?limit=1&after="+first.NextAfter)
	require.Equal(t, http.StatusOK, second.Code, second.Body.String())
	var remaining api.PackagePage
	require.NoError(t, json.Unmarshal(second.Body.Bytes(), &remaining))
	require.Len(t, remaining.Items, 1)
	assert.ElementsMatch(t, []string{received.PackageID, produced.PackageID},
		[]string{first.Items[0].PackageID, remaining.Items[0].PackageID})

	detail := srv.get(t, "/api/v1/packages/by-id/"+received.PackageID)
	require.Equal(t, http.StatusOK, detail.Code, detail.Body.String())
	var packageDetail store.Package
	require.NoError(t, json.Unmarshal(detail.Body.Bytes(), &packageDetail))
	assert.Equal(t, received.PackageID, packageDetail.PackageID)
	assert.Equal(t, "received", packageDetail.Direction)

	members := srv.get(t, "/api/v1/packages/by-id/"+produced.PackageID+"/members?limit=1")
	require.Equal(t, http.StatusOK, members.Code, members.Body.String())
	var memberPage api.PackageMemberPage
	require.NoError(t, json.Unmarshal(members.Body.Bytes(), &memberPage))
	require.Len(t, memberPage.Items, 1)
	assert.Equal(t, 1, memberPage.Items[0].Ordinal)
	assert.Equal(t, "produced.txt", memberPage.Items[0].DisplayName)

	labels := srv.get(t, "/api/v1/packages/label-candidates?label=EXT000001&package_id="+
		received.PackageID+"&label_set=sender&provenance=received&limit=1")
	require.Equal(t, http.StatusOK, labels.Code, labels.Body.String())
	var labelPage api.PackageLabelCandidatePage
	require.NoError(t, json.Unmarshal(labels.Body.Bytes(), &labelPage))
	require.Len(t, labelPage.Items, 1)
	assert.Equal(t, received.PackageID, labelPage.Items[0].PackageID)
	assert.Equal(t, "EXT000001", labelPage.Items[0].Label)
	assert.Empty(t, labelPage.NextCursor)

	timeline := srv.get(t, "/api/v1/packages/by-id/"+received.PackageID+"/timeline-inputs?limit=1")
	require.Equal(t, http.StatusOK, timeline.Code, timeline.Body.String())
	var timelinePage api.PackageTimelineInputPage
	require.NoError(t, json.Unmarshal(timeline.Body.Bytes(), &timelinePage))
	require.Len(t, timelinePage.Items, 1)
	assert.Equal(t, rowID, timelinePage.Items[0].RowID)
	assert.Equal(t, raw, timelinePage.Items[0].RawJSON)
	assert.Equal(t, "UTC", timelinePage.Items[0].DeclaredTimezone)

	record := srv.get(t, "/api/v1/packages/by-id/"+received.PackageID+"/records/"+rowID)
	require.Equal(t, http.StatusOK, record.Code, record.Body.String())
	var recordDetail api.PackageRecord
	require.NoError(t, json.Unmarshal(record.Body.Bytes(), &recordDetail))
	assert.Equal(t, rowID, recordDetail.RowID)
	assert.Equal(t, raw, recordDetail.RawJSON)
	assert.Equal(t, "VOL001/DATA/received.dat", recordDetail.LoadFile)

	catalogResponse := srv.get(t, "/api/v1/packages/field-catalog")
	require.Equal(t, http.StatusOK, catalogResponse.Code, catalogResponse.Body.String())
	var fieldCatalog api.PackageFieldCatalog
	require.NoError(t, json.Unmarshal(catalogResponse.Body.Bytes(), &fieldCatalog))
	assert.NotEmpty(t, fieldCatalog.Fields)
	assert.Contains(t, fieldCatalog.Fields, loadfile.CatalogEntry{
		Canonical: "loadfile.label.begin", Aliases: []string{"BEGBATES", "BEGDOC", "Begin Doc"},
	})
}

func TestPackageRecordRouteWithholdsSensitiveRowsFromBrowserSessions(t *testing.T) {
	srv, catalog := newPackageTestServer(t)
	received, rowID, raw := seedBrowseReceivedPackage(t, catalog, true)

	path := "/api/v1/packages/by-id/" + received.PackageID + "/records/" + rowID
	master := srv.get(t, path)
	require.Equal(t, http.StatusOK, master.Code, master.Body.String())
	var full api.PackageRecord
	require.NoError(t, json.Unmarshal(master.Body.Bytes(), &full))
	assert.Equal(t, raw, full.RawJSON)

	browser := srv.call(t, http.MethodGet, path, "",
		map[string]string{"X-Api-Key": "", api.WebSessionHeader: packageBrowserToken(t, srv)})
	require.Equal(t, http.StatusOK, browser.Code, browser.Body.String())
	var redacted api.PackageRecord
	require.NoError(t, json.Unmarshal(browser.Body.Bytes(), &redacted))
	assert.True(t, redacted.Sensitive)
	assert.Empty(t, redacted.RawJSON)
}

func TestPackageBrowseRoutesEnforceBoundsAndIdentity(t *testing.T) {
	srv, _ := newPackageTestServer(t)
	for _, path := range []string{
		"/api/v1/packages?limit=251",
		"/api/v1/packages/by-id/" + uuid.NewString() + "/members?after_ordinal=-1",
		"/api/v1/packages/label-candidates",
		"/api/v1/packages/label-candidates?label=EXT000001&limit=0",
		"/api/v1/packages/by-id/" + uuid.NewString() + "/timeline-inputs?limit=251",
		"/api/v1/packages/by-id/" + uuid.NewString() + "/records/not-a-record-id",
	} {
		response := srv.get(t, path)
		assert.Equal(t, http.StatusUnprocessableEntity, response.Code, "%s: %s", path, response.Body.String())
	}
	missing := srv.get(t, "/api/v1/packages/by-id/"+uuid.NewString())
	assert.Equal(t, http.StatusNotFound, missing.Code, missing.Body.String())
}

func TestPackageCustodianAndPeopleRoutesReturnCandidatesBeforeExactMutation(t *testing.T) {
	srv, catalog := newPackageTestServer(t)
	pkg, rowID, _ := seedBrowseReceivedPackage(t, catalog, false)
	person, err := catalog.CreatePerson(t.Context(), "Synthetic Custodian", "operator")
	require.NoError(t, err)
	assignment, err := catalog.SetCustodian(t.Context(), store.CustodianRequest{
		Scope:    store.CustodianScope{Kind: "package", PackageID: pkg.PackageID, PackageRecordID: rowID, HasPackageRecordID: true},
		RawLabel: "Sender Owner", Rank: "primary", Basis: "package_column", SourceRef: "CUSTODIAN", IfMatchRevision: 1,
	})
	require.NoError(t, err)
	packageDefault, err := catalog.SetCustodian(t.Context(), store.CustodianRequest{
		Scope:    store.CustodianScope{Kind: "package", PackageID: pkg.PackageID},
		RawLabel: "Package Default", Rank: "primary", Basis: "transfer_record", SourceRef: "receipt", IfMatchRevision: 1,
	})
	require.NoError(t, err)

	people := srv.get(t, "/api/v1/people?query=synthetic&limit=10")
	require.Equal(t, http.StatusOK, people.Code, people.Body.String())
	var peoplePage api.PersonPage
	require.NoError(t, json.Unmarshal(people.Body.Bytes(), &peoplePage))
	require.Len(t, peoplePage.Items, 1)
	assert.Equal(t, person.PersonID, peoplePage.Items[0].PersonID)

	custodians := srv.get(t, "/api/v1/packages/by-id/"+pkg.PackageID+"/custodians?row_id="+rowID+"&unresolved_only=true&limit=10")
	require.Equal(t, http.StatusOK, custodians.Code, custodians.Body.String())
	var custodianPage api.CustodianPage
	require.NoError(t, json.Unmarshal(custodians.Body.Bytes(), &custodianPage))
	require.Len(t, custodianPage.Items, 1)
	assert.Equal(t, assignment.AssignmentID, custodianPage.Items[0].AssignmentID)
	defaults := srv.get(t, "/api/v1/packages/by-id/"+pkg.PackageID+"/custodians?limit=10")
	require.Equal(t, http.StatusOK, defaults.Code, defaults.Body.String())
	require.NoError(t, json.Unmarshal(defaults.Body.Bytes(), &custodianPage))
	require.Len(t, custodianPage.Items, 1)
	assert.Equal(t, packageDefault.AssignmentID, custodianPage.Items[0].AssignmentID)

	resolved := srv.call(t, http.MethodPost, "/api/v1/packages/custodians/"+assignment.AssignmentID+"/resolve",
		mustPackageJSON(t, api.CustodianResolveRequest{PersonID: person.PersonID, IfMatchRevision: assignment.Revision}), nil)
	require.Equal(t, http.StatusOK, resolved.Code, resolved.Body.String())
	var linked api.CustodianAssignment
	require.NoError(t, json.Unmarshal(resolved.Body.Bytes(), &linked))
	assert.Equal(t, person.PersonID, linked.PersonID)
	assert.Equal(t, "Sender Owner", linked.RawLabel)

	operator := srv.call(t, http.MethodPut, "/api/v1/packages/by-id/"+pkg.PackageID+"/custodian",
		mustPackageJSON(t, api.PackageCustodianRequest{RowID: new(rowID), RawLabel: "Operator", IfMatchRevision: 1}), nil)
	assert.Equal(t, http.StatusConflict, operator.Code, operator.Body.String())
}

func seedBrowseReceivedPackage(t *testing.T, catalog *testStore, sensitive bool) (store.Package, string, []byte) {
	t.Helper()
	profile, err := loadfile.ReadProfile("dat-concordance-v1")
	require.NoError(t, err)
	profile.DeclaredTimezone = "UTC"
	profileJSON, err := canonical.Marshal(profile)
	require.NoError(t, err)
	profileDigest := sha256.Sum256(profileJSON)
	mappingJSON := []byte("{}")
	mappingDigest := sha256.Sum256(mappingJSON)
	manifestHash, manifestSize, err := catalog.Blobs.Write(strings.NewReader("synthetic received manifest"))
	require.NoError(t, err)
	require.NoError(t, catalog.RecordBlob(t.Context(), manifestHash, manifestSize,
		store.BlobPhysical{Encoding: "raw", StoredBytes: manifestSize}))
	run, err := catalog.BeginIngest(t.Context(), "package:loadfile", "synthetic received package")
	require.NoError(t, err)
	body := []byte("synthetic received document")
	bodyDigest := sha256.Sum256(body)
	node, err := catalog.IngestFileExact(t.Context(), run, catalog.RootID(), "received.txt",
		hex.EncodeToString(bodyDigest[:]), int64(len(body)), "text/plain", "received.txt", "")
	require.NoError(t, err)
	request := store.PackageRequest{
		PackageID: uuid.NewString(), Direction: "received", PackageName: "received-001",
		PartyLabel: "Synthetic sender", ProfileJSON: string(profileJSON),
		ProfileSHA256: hex.EncodeToString(profileDigest[:]), MappingJSON: string(mappingJSON),
		MappingSHA256: hex.EncodeToString(mappingDigest[:]), ManifestSHA256: manifestHash,
		ManifestBlobSHA256: manifestHash, IngestID: run.ID(), ProducedOn: "2026-09-21T00:00:00Z",
		State: "importing", Volumes: []store.PackageVolume{{Ordinal: 1, VolumeName: "VOL001",
			DeclaredRoot: "VOL001", MappedRoot: "VOL001", ResolvedRootSHA256: testHash("received-root")}},
	}
	preflightID := uuid.NewString()
	owner := "synthetic-operator"
	_, err = catalog.PutPackagePreflight(t.Context(), store.PackagePreflightRecord{
		PreflightID: preflightID, Owner: owner, SourceKind: "root", SourceRef: "synthetic-root-digest",
		SourceLocator: "synthetic-root", ProfileJSON: request.ProfileJSON, MappingJSON: request.MappingJSON,
		ProfileSHA256: request.ProfileSHA256, MappingSHA256: request.MappingSHA256,
		ManifestSHA256: request.ManifestSHA256, ManifestBlobSHA256: request.ManifestBlobSHA256,
		CanonicalJSON: []byte(`{}`), DiagnosticsJSON: []byte(`[]`),
	})
	require.NoError(t, err)
	_, err = catalog.AdmitPackageImport(t.Context(), run, request, store.PackageImportJobRequest{
		ID: uuid.NewString(), Owner: owner, OperationID: uuid.NewString(), RequestSHA256: testHash("synthetic import"),
		PreflightID: preflightID, PackageID: request.PackageID,
		JobJSON: []byte(`{"source_kind":"root","source_locator":"synthetic-root"}`),
	})
	require.NoError(t, err)
	pkg, err := catalog.Package(t.Context(), request.PackageID)
	require.NoError(t, err)
	job, err := catalog.ClaimPackageImportJob(t.Context(), "browse-worker", time.Minute)
	require.NoError(t, err)
	rowID, err := store.PackageRecordKey("VOL001/DATA/received.dat", 1, "DOC-A")
	require.NoError(t, err)
	occurrenceID := store.PackageOccurrenceID(pkg.PackageID, rowID)
	raw := []byte(`{"BEGBATES":"EXT000001","DOCDATE":"2026-09-21"}`)
	rawDigest := sha256.Sum256(raw)
	_, err = catalog.CommitPackageRecordWithLease(t.Context(), job.ID, job.Epoch, job.Token, store.PackageRecordRow{
		PackageID: pkg.PackageID, RowID: rowID, LoadFile: "VOL001/DATA/received.dat", RowOrdinal: 1,
		OccurrenceID: occurrenceID, RawJSON: raw, RawSHA256: hex.EncodeToString(rawDigest[:]), Sensitive: sensitive,
	}, []store.PackageLabelRow{{
		PackageID: pkg.PackageID, Provenance: "received", LabelSet: "sender", Label: "EXT000001",
		LabelSortKey: store.LabelSortKey("EXT000001"), OccurrenceID: occurrenceID,
		ContentVersionID: node.CurrentVersionID, PageState: "unknown", Endpoint: "begin",
	}}, store.PackageImportReceipt{
		ReceiptID: uuid.NewString(), PackageID: pkg.PackageID, RecordKey: rowID, OccurrenceID: occurrenceID,
		ContentVersionID: node.CurrentVersionID, State: "committed", ReceiptJSON: []byte("{}"),
	})
	require.NoError(t, err)
	return pkg, rowID, raw
}

func seedBrowseProducedPackage(t *testing.T, catalog *testStore) store.Package {
	t.Helper()
	body := "synthetic produced document"
	hash, size, err := catalog.Blobs.Write(strings.NewReader(body))
	require.NoError(t, err)
	node, err := catalog.CreateFile(t.Context(), catalog.RootID(), "produced.txt", hash, size, "text/plain")
	require.NoError(t, err)
	snapshotID := uuid.NewString()
	occurrenceID := strings.Repeat("a", 32)
	_, err = catalog.SealCollectionSnapshot(t.Context(), store.SnapshotSealRequest{
		SnapshotID: snapshotID, Members: []store.CollectionSnapshotMember{{
			Ordinal: 1, OccurrenceID: occurrenceID, NodeID: node.ID, ContentVersionID: node.CurrentVersionID,
			BlobSHA256: hash, Size: size, FamilyID: occurrenceID, FamilyOrder: 1,
			DisplayName: "produced.txt", FrozenFieldsJSON: "{}", DocumentKind: "other",
		}},
	})
	require.NoError(t, err)
	manifestHash, manifestSize, err := catalog.Blobs.Write(strings.NewReader("synthetic produced manifest"))
	require.NoError(t, err)
	require.NoError(t, catalog.RecordBlob(t.Context(), manifestHash, manifestSize,
		store.BlobPhysical{Encoding: "raw", StoredBytes: manifestSize}))
	profileJSON := []byte("{}")
	profileDigest := sha256.Sum256(profileJSON)
	request := store.PackageRequest{
		PackageID: uuid.NewString(), SnapshotID: snapshotID, Direction: "produced", PackageName: "produced-001",
		PartyLabel: "Synthetic recipient", ProfileJSON: string(profileJSON),
		ProfileSHA256: hex.EncodeToString(profileDigest[:]), MappingJSON: string(profileJSON),
		MappingSHA256: hex.EncodeToString(profileDigest[:]), ManifestSHA256: manifestHash,
		ManifestBlobSHA256: manifestHash, State: "sealed", Volumes: []store.PackageVolume{{Ordinal: 1,
			VolumeName: "VOL001", DeclaredRoot: "VOL001", MappedRoot: "VOL001",
			ResolvedRootSHA256: testHash("produced-root")}},
	}
	pkg, err := catalog.CreatePackage(t.Context(), request)
	require.NoError(t, err)
	return pkg
}
