package store

import (
	"bytes"
	"database/sql"
	"encoding/json/v2"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document"
)

func TestBackupExcludesTheProjectionButShipsBindings(t *testing.T) {
	source := newTestStore(t)
	version, target := ingestDocumentEventTarget(t, source, "bound.txt", "a1")
	_, err := source.db.Exec(`INSERT INTO document_event_state(
		singleton,contract_version,deriver_fingerprint,input_epoch,publication_epoch,updated_at
	) VALUES(1,?,?,?,?,?)`, document.DocumentEventsContractV1, fakeHash("f1"), 1, 1,
		"2020-01-01T00:00:00Z")
	require.NoError(t, err)
	record := documentEventRecord(t, source.VaultID(), version.ID, "b1")
	_, err = source.PublishDocumentEvents(t.Context(), target, fakeHash("f1"),
		requireDocumentEventInputsSHA256(t, source, target), mustMarshalDocumentEvents(t, record))
	require.NoError(t, err)
	sourceBindings, err := source.ProvenanceVersionBindings(t.Context(), version.ID)
	require.NoError(t, err)
	require.Len(t, sourceBindings, 1)

	var exported bytes.Buffer
	require.NoError(t, source.ExportMetadata(t.Context(), &exported))
	for _, derivedType := range []string{
		"document_event_state",
		"document_event_generation",
		"document_event_head",
		"document_event",
		"document_event_actor",
		"document_event_primary",
		"document_event_build",
		"document_event_dirty",
		"document_event_attempt",
	} {
		assert.NotContains(t, exported.String(), `"type":"`+derivedType+`"`)
	}

	lines := bytes.Split(bytes.TrimSpace(exported.Bytes()), []byte{'\n'})
	provenanceIndex, bindingIndex, bindingCount := -1, -1, 0
	for index, line := range lines {
		var header struct {
			Type string `json:"type"`
		}
		require.NoError(t, json.Unmarshal(line, &header))
		switch header.Type {
		case metadataProvenanceType:
			provenanceIndex = index
		case metadataProvenanceVersionBindingType:
			bindingIndex = index
			bindingCount++
		}
	}
	require.Equal(t, 1, bindingCount)
	assert.Equal(t, provenanceIndex+1, bindingIndex,
		"bindings must immediately follow provenance records")

	restored := newTestStore(t)
	require.NoError(t, restored.ImportMetadata(t.Context(), bytes.NewReader(exported.Bytes())))
	bindings, err := restored.ProvenanceVersionBindings(t.Context(), version.ID)
	require.NoError(t, err)
	assert.Equal(t, sourceBindings, bindings)

	missing, err := restored.MissingDocumentEventTargetsAfter(t.Context(), fakeHash("f1"), "", 10)
	require.NoError(t, err)
	require.Len(t, missing, 1)
	assert.Equal(t, version.ID, missing[0].ContentVersionID)
	var stateRows int
	require.NoError(t, restored.db.QueryRow(`SELECT COUNT(*) FROM document_event_state`).Scan(&stateRows))
	assert.Zero(t, stateRows, "read-only coverage must keep event control state lazy")
}

func TestPristineCheckRefusesAnExistingProjection(t *testing.T) {
	source := newTestStore(t)
	var exported bytes.Buffer
	require.NoError(t, source.ExportMetadata(t.Context(), &exported))

	target := newTestStore(t)
	_, err := target.db.Exec(`INSERT INTO document_event_state(
		singleton,contract_version,deriver_fingerprint,input_epoch,publication_epoch,updated_at
	) VALUES(1,'document-events/v1','test',1,1,'2020-01-01T00:00:00Z')`)
	require.NoError(t, err)
	err = target.ImportMetadata(t.Context(), bytes.NewReader(exported.Bytes()))
	require.ErrorContains(t, err, "metadata import target is not pristine")
}

func TestPristineTargetCountsEventControlRows(t *testing.T) {
	s := newTestStore(t)
	_, err := s.db.Exec(`INSERT INTO document_event_state(
		singleton,contract_version,deriver_fingerprint,input_epoch,publication_epoch,updated_at
	) VALUES(1,'document-events/v1','test',1,1,'2020-01-01T00:00:00Z')`)
	require.NoError(t, err)
	err = s.withStorageTx(t.Context(), func(tx *sql.Tx) error {
		return requirePristineMetadataTarget(t.Context(), tx)
	})
	require.ErrorContains(t, err, "not pristine")
}
