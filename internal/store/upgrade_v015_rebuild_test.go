package store

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/internal/canonical"
)

// driveDocumentEventWorker does what the daemon's document event backfill
// does on each pass: install the recipe, derive every missing target, and
// refresh running receipts.
func driveDocumentEventWorker(t *testing.T, s *Store) {
	t.Helper()
	ctx := t.Context()
	require.NoError(t, s.EnsureDocumentEventRecipe(ctx, DocumentEventsDeriverFingerprint))
	targets, err := s.MissingDocumentEventTargetsAfter(ctx, DocumentEventsDeriverFingerprint, "", 100)
	require.NoError(t, err)
	for _, target := range targets {
		_, err := s.PublishDocumentEvents(ctx, target, DocumentEventsDeriverFingerprint,
			requireDocumentEventInputsSHA256(t, s, target),
			mustMarshalDocumentEvents(t, documentEventRecord(t, s.VaultID(), target.ContentVersionID, "e")))
		require.NoError(t, err)
	}
	require.NoError(t, s.RefreshDocumentEventBuilds(ctx))
}

// driveDocumentPeopleWorker does what the daemon's people backfill does on
// each pass, publishing an attribution with no edges for every target.
func driveDocumentPeopleWorker(t *testing.T, s *Store) {
	t.Helper()
	ctx := t.Context()
	targets, err := s.MissingDocumentPeopleTargetsAfter(ctx, document.PersonResolverFingerprint(), "", 100)
	require.NoError(t, err)
	for _, target := range targets {
		input, err := s.PrepareDocumentPeopleInputs(ctx, target.ContentVersionID)
		require.NoError(t, err)
		raw, err := canonical.Marshal(input)
		require.NoError(t, err)
		_, err = s.PublishDocumentPeople(ctx, DocumentPeoplePublication{
			People: document.DocumentPeopleV1{ContractVersion: document.PersonContractV1,
				ContentVersionID: input.ContentVersionID, EventGenerationID: input.EventGenerationID,
				Edges: []document.DocumentPersonEdgeV1{}},
			InputsSHA256: fakeSHA256(raw), ResolverFingerprint: document.PersonResolverFingerprint(),
			NodeID: input.NodeID, BindingEpoch: input.BindingEpoch, NodeRevision: input.NodeRevision,
		})
		require.NoError(t, err)
	}
	require.NoError(t, s.RefreshDocumentPeopleBuilds(ctx))
}

func rebuildStateRows(t *testing.T, s *Store) string {
	t.Helper()
	var out strings.Builder
	for _, query := range []string{
		`SELECT deriver_fingerprint,input_epoch,publication_epoch FROM document_event_state`,
		`SELECT resolver_fingerprint,binding_epoch,publication_epoch FROM document_people_state`,
	} {
		var fingerprint string
		var epoch, publication int64
		err := s.db.QueryRowContext(t.Context(), query).Scan(&fingerprint, &epoch, &publication)
		if errors.Is(err, sql.ErrNoRows) {
			out.WriteString("(no row); ")
			continue
		}
		require.NoError(t, err)
		fmt.Fprintf(&out, "fingerprint=%s epoch=%d publication=%d; ", fingerprint, epoch, publication)
	}
	return out.String()
}

func TestUpgradeReleasedV0150KeepsRebuildReceipts(t *testing.T) {
	t.Parallel()
	const eventDone, eventRunning = "40000000-0000-4000-8000-0000000000e1", "40000000-0000-4000-8000-0000000000e2"
	const peopleDone, peopleRunning = "40000000-0000-4000-8000-0000000000a1", "40000000-0000-4000-8000-0000000000a2"
	for _, driver := range v090UpgradeDrivers() {
		t.Run(driver.name, func(t *testing.T) {
			ctx := t.Context()
			path := filepath.Join(t.TempDir(), "docbank.db")
			db, legacy := newReleasedFixtureStore(t, path, driver.driver, schemaV0150SQL, 28)
			ingestDocumentEventTarget(t, legacy, "one.txt", "f1")
			ingestDocumentEventTarget(t, legacy, "two.txt", "f2")

			// Seed the v0.15 receipt and state tables directly, as in the released
			// date fixture: current event publication also writes a newer table.
			// Completed counts can outlive their targets: version deletion cascades
			// generations and heads, while receipts have no version foreign key.
			// These two retained targets await the later running rebuilds.
			oldSum := sha256.Sum256([]byte("docbank-document-events:f10-metadata+content-version+provenance-binding+actor-evidence:v2"))
			oldFingerprint := hex.EncodeToString(oldSum[:])
			resolver := document.PersonResolverFingerprint()
			const timestamp = "2026-01-01T00:00:00Z"
			_, err := db.ExecContext(ctx, `INSERT INTO document_event_state(singleton,contract_version,deriver_fingerprint,input_epoch,publication_epoch,updated_at) VALUES(1,$1,$2,3,3,$3)`, document.DocumentEventsContractV1, oldFingerprint, timestamp)
			require.NoError(t, err)
			_, err = db.ExecContext(ctx, `UPDATE document_people_state SET resolver_fingerprint=$1,binding_epoch=3,publication_epoch=3,updated_at=$2 WHERE singleton=1`, resolver, timestamp)
			require.NoError(t, err)
			_, err = db.ExecContext(ctx, `INSERT INTO document_event_builds(operation_id,request_sha256,deriver_fingerprint,state,target_epoch,scanned,published,unavailable,failed,started_at,updated_at,finished_at) VALUES($1,$2,$3,'completed',2,2,2,0,0,$4,$4,$4),($5,$2,$3,'running',3,0,0,0,0,$4,$4,NULL)`, eventDone, DocumentEventRebuildRequestSHA256, oldFingerprint, timestamp, eventRunning)
			require.NoError(t, err)
			_, err = db.ExecContext(ctx, `INSERT INTO document_people_builds(operation_id,request_sha256,resolver_fingerprint,state,target_epoch,scanned,published,failed,started_at,updated_at,finished_at) VALUES($1,$2,$3,'completed',2,2,2,0,$4,$4,$4),($5,$2,$3,'running',3,0,0,0,$4,$4,'')`, peopleDone, documentPeopleRebuildDigest(resolver), resolver, timestamp, peopleRunning)
			require.NoError(t, err)

			events := map[string]DocumentEventBuild{}
			for _, id := range []string{eventDone, eventRunning} {
				events[id], err = legacy.DocumentEventBuild(ctx, id)
				require.NoError(t, err)
			}
			people := map[string]DocumentPeopleBuild{}
			for _, id := range []string{peopleDone, peopleRunning} {
				people[id], err = legacy.DocumentPeopleBuild(ctx, id)
				require.NoError(t, err)
			}
			require.Equal(t, "completed", events[eventDone].State)
			require.Equal(t, int64(2), events[eventDone].Published)
			require.Equal(t, "completed", people[peopleDone].State)
			require.Equal(t, int64(2), people[peopleDone].Published)
			require.Equal(t, "running", events[eventRunning].State)
			require.Equal(t, "running", people[peopleRunning].State)
			sourceState := rebuildStateRows(t, legacy)
			require.NoError(t, db.Close())

			s, err := Open(t.Context(), path, driver.driver)
			require.NoError(t, err)
			defer func() { require.NoError(t, s.Close()) }()
			for id, released := range events {
				upgraded, err := s.DocumentEventBuild(ctx, id)
				require.NoError(t, err, "event receipt %s survives", id)
				assert.Equal(t, released, upgraded)
			}
			for id, released := range people {
				upgraded, err := s.DocumentPeopleBuild(ctx, id)
				require.NoError(t, err, "people receipt %s survives", id)
				assert.Equal(t, released, upgraded)
			}
			assert.Equal(t, sourceState, rebuildStateRows(t, s))

			replayedEvent, err := s.StartDocumentEventRebuild(ctx, eventDone, DocumentEventRebuildRequestSHA256)
			require.NoError(t, err)
			assert.Equal(t, events[eventDone], replayedEvent, "replay returns the saved receipt")
			replayedPeople, err := s.RebuildDocumentPeople(ctx, peopleDone)
			require.NoError(t, err)
			assert.Equal(t, people[peopleDone], replayedPeople, "replay returns the saved receipt")
			assert.Equal(t, sourceState, rebuildStateRows(t, s), "replay starts no new rebuild")

			for range 2 {
				driveDocumentEventWorker(t, s)
				driveDocumentPeopleWorker(t, s)
			}
			event, err := s.DocumentEventBuild(ctx, eventRunning)
			require.NoError(t, err)
			assert.Equal(t, "completed", event.State, "running event rebuild finishes")
			assert.Equal(t, int64(2), event.Published)
			resumed, err := s.DocumentPeopleBuild(ctx, peopleRunning)
			require.NoError(t, err)
			assert.Equal(t, "completed", resumed.State, "running people rebuild finishes")
			assert.Equal(t, int64(2), resumed.Published)
			doneEvent, err := s.DocumentEventBuild(ctx, eventDone)
			require.NoError(t, err)
			assert.Equal(t, events[eventDone], doneEvent, "completed receipt keeps its counts")
			donePeople, err := s.DocumentPeopleBuild(ctx, peopleDone)
			require.NoError(t, err)
			assert.Equal(t, people[peopleDone], donePeople, "completed receipt keeps its counts")
		})
	}
}
