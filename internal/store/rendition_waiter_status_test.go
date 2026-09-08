package store

import (
	"bytes"
	"database/sql"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

type renditionWaiterStatusFixture struct {
	store         *Store
	profile       ProcessingProfileRecord
	job           RenditionJob
	firstRequest  RenditionJobRequest
	secondRequest RenditionJobRequest
	firstWaiter   RenditionJobWaiter
	secondWaiter  RenditionJobWaiter
}

func newRenditionWaiterStatusFixture(t *testing.T) renditionWaiterStatusFixture {
	t.Helper()
	s, versions := newRenditionCatalogFixture(t)
	profile := catalogProcessingProfile(t, false)
	firstRequest := renditionJobTestRequest(versions[0], profile)
	firstRequest.Authorization.Principal = "operator:waiter-status-first"
	secondRequest := renditionJobTestRequest(versions[1], profile)
	secondRequest.Authorization.Principal = "operator:waiter-status-second"
	grantRenditionJobConsent(t, s, firstRequest)
	grantRenditionJobConsent(t, s, secondRequest)
	job, firstWaiter, err := s.EnqueueRenditionJob(t.Context(), firstRequest)
	require.NoError(t, err)
	_, secondWaiter, err := s.EnqueueRenditionJob(t.Context(), secondRequest)
	require.NoError(t, err)
	return renditionWaiterStatusFixture{
		store: s, profile: profile, job: job,
		firstRequest: firstRequest, secondRequest: secondRequest,
		firstWaiter: firstWaiter, secondWaiter: secondWaiter,
	}
}

func (f renditionWaiterStatusFixture) orderedWaiters() (
	RenditionJobRequest, RenditionJobWaiter, RenditionJobRequest, RenditionJobWaiter,
) {
	if f.firstWaiter.ID < f.secondWaiter.ID {
		return f.firstRequest, f.firstWaiter, f.secondRequest, f.secondWaiter
	}
	return f.secondRequest, f.secondWaiter, f.firstRequest, f.firstWaiter
}

func (f renditionWaiterStatusFixture) waitersForSelected(selectedID string) (
	RenditionJobRequest, RenditionJobWaiter, RenditionJobRequest, RenditionJobWaiter,
) {
	if selectedID == f.firstWaiter.ID {
		return f.firstRequest, f.firstWaiter, f.secondRequest, f.secondWaiter
	}
	return f.secondRequest, f.secondWaiter, f.firstRequest, f.firstWaiter
}

func invalidateRenditionWaiterAuthority(
	t *testing.T, s *Store, request RenditionJobRequest, cause RenditionFailureCode,
) {
	t.Helper()
	switch cause {
	case RenditionFailureConsent:
		_, err := s.RevokeConsent(t.Context(), ProcessingConsentRevocationRequest{
			Principal: request.Authorization.Principal,
			Scope:     request.Authorization.Scope,
		})
		require.NoError(t, err)
	case RenditionFailureStaleAuthority:
		driftedHash := testSHA256([]byte("synthetic waiter status authority drift" + request.ContentVersionID))
		require.NoError(t, s.withStorageTx(t.Context(), func(tx *sql.Tx) error {
			if err := s.EnsureBlobTx(tx, driftedHash, 20); err != nil {
				return err
			}
			_, err := tx.ExecContext(t.Context(),
				`UPDATE content_versions SET blob_hash=? WHERE version_id=?`,
				driftedHash, request.ContentVersionID)
			return err
		}))
	default:
		require.FailNow(t, "unsupported synthetic waiter failure", string(cause))
	}
}

func TestRenditionJobWaiterByIDBoundsAndValidatesStatus(t *testing.T) {
	fixture := newRenditionWaiterStatusFixture(t)
	waiting, err := fixture.store.RenditionJobWaiterByID(t.Context(), fixture.firstWaiter.ID)
	require.NoError(t, err)
	require.Equal(t, fixture.firstWaiter.ID, waiting.ID)
	require.Equal(t, "waiting", waiting.State)
	require.Empty(t, waiting.FailureCode)

	_, err = fixture.store.RenditionJobWaiterByID(t.Context(), "not-a-sha256")
	require.ErrorIs(t, err, ErrNotFound)
	_, err = fixture.store.RenditionJobWaiterByID(
		t.Context(), testSHA256([]byte("missing synthetic waiter")))
	require.ErrorIs(t, err, ErrNotFound)

	t.Run("legacy rejected null is unknown", func(t *testing.T) {
		_, err := fixture.store.db.ExecContext(t.Context(), `UPDATE rendition_job_waiters
			SET state='rejected',failure_code=NULL WHERE waiter_id=?`, fixture.firstWaiter.ID)
		require.NoError(t, err)
		legacy, err := fixture.store.RenditionJobWaiterByID(t.Context(), fixture.firstWaiter.ID)
		require.NoError(t, err)
		require.Equal(t, "rejected", legacy.State)
		require.Empty(t, legacy.FailureCode)
	})

	for _, test := range []struct {
		name  string
		state string
		code  string
	}{
		{name: "rejected unknown", state: "rejected", code: "unknown"},
		{name: "rejected empty nonnull", state: "rejected", code: ""},
		{name: "waiting consent", state: "waiting", code: "consent"},
		{name: "published stale authority", state: "published", code: "stale_authority"},
	} {
		t.Run(test.name, func(t *testing.T) {
			_, err := fixture.store.db.ExecContext(t.Context(), `UPDATE rendition_job_waiters
				SET state=?,failure_code=? WHERE waiter_id=?`,
				test.state, test.code, fixture.firstWaiter.ID)
			require.NoError(t, err)
			_, err = fixture.store.RenditionJobWaiterByID(t.Context(), fixture.firstWaiter.ID)
			require.ErrorContains(t, err, "invalid durable failure")
		})
	}

	_, err = fixture.store.db.ExecContext(t.Context(), `UPDATE rendition_job_waiters
		SET state='corrupt',failure_code=NULL WHERE waiter_id=?`, fixture.firstWaiter.ID)
	require.NoError(t, err)
	_, err = fixture.store.RenditionJobWaiterByID(t.Context(), fixture.firstWaiter.ID)
	require.ErrorContains(t, err, "invalid durable state")
}

func TestRenditionWaiterFailureClearsOnRenewedConsentAndPublication(t *testing.T) {
	s, versions := newRenditionCatalogFixture(t)
	profile := catalogProcessingProfile(t, false)
	request := renditionJobTestRequest(versions[0], profile)
	grantRenditionJobConsent(t, s, request)
	job, waiter, err := s.EnqueueRenditionJob(t.Context(), request)
	require.NoError(t, err)
	waiting, err := s.RenditionJobWaiterByID(t.Context(), waiter.ID)
	require.NoError(t, err)
	require.Equal(t, "waiting", waiting.State)
	require.Empty(t, waiting.FailureCode)

	now := time.Now().UTC().Add(time.Second)
	claim, err := s.ClaimRenditionJob(t.Context(), job.ID, "worker:waiter-reset", now, time.Minute)
	require.NoError(t, err)
	_, err = s.RevokeConsent(t.Context(), ProcessingConsentRevocationRequest{
		Principal: request.Authorization.Principal, Scope: request.Authorization.Scope,
	})
	require.NoError(t, err)
	_, err = s.RenditionJobWorkByClaim(t.Context(), claim, now.Add(time.Second))
	require.ErrorIs(t, err, ErrProcessingConsentRequired)
	rejected, err := s.RenditionJobWaiterByID(t.Context(), waiter.ID)
	require.NoError(t, err)
	require.Equal(t, RenditionFailureConsent, rejected.FailureCode)

	grantRenditionJobConsent(t, s, request)
	_, renewed, err := s.EnqueueRenditionJob(t.Context(), request)
	require.NoError(t, err)
	require.Equal(t, waiter.ID, renewed.ID)
	renewed, err = s.RenditionJobWaiterByID(t.Context(), waiter.ID)
	require.NoError(t, err)
	require.Equal(t, "waiting", renewed.State)
	require.Empty(t, renewed.FailureCode)

	work, err := s.RenditionJobWorkByClaim(t.Context(), claim, now.Add(2*time.Second))
	require.NoError(t, err)
	_, err = s.BeginRenditionProvider(t.Context(), claim, work.Waiter.ID,
		now.Add(3*time.Second), renditionJobTestSnapshot(request))
	require.NoError(t, err)
	build := catalogRenditionBuild(s, profile)
	build.ID = job.ID
	require.NoError(t, s.StageRenditionJobBuild(t.Context(), claim, build, now.Add(4*time.Second)))
	_, err = s.StageRenditionJobGeneration(t.Context(), claim,
		testSHA256([]byte("waiter status reset generation")), now.Add(5*time.Second))
	require.NoError(t, err)
	_, err = s.PublishRenditionJob(t.Context(), claim, now.Add(6*time.Second))
	require.NoError(t, err)
	published, err := s.RenditionJobWaiterByID(t.Context(), waiter.ID)
	require.NoError(t, err)
	require.Equal(t, "published", published.State)
	require.Empty(t, published.FailureCode)

	_, joined, err := s.EnqueueRenditionJob(t.Context(), request)
	require.NoError(t, err)
	joined, err = s.RenditionJobWaiterByID(t.Context(), joined.ID)
	require.NoError(t, err)
	require.Equal(t, "published", joined.State)
	require.Empty(t, joined.FailureCode)
}

func TestRenditionWaiterFailureMetadataRoundTripAndOptionalCompatibility(t *testing.T) {
	for _, cause := range []RenditionFailureCode{
		RenditionFailureConsent, RenditionFailureStaleAuthority,
	} {
		t.Run(string(cause), func(t *testing.T) {
			mixed := renditionWaiterMixedMetadataFixture(t, cause)
			var first, second bytes.Buffer
			require.NoError(t, mixed.fixture.store.ExportMetadata(t.Context(), &first))
			require.NoError(t, mixed.fixture.store.ExportMetadata(t.Context(), &second))
			require.Equal(t, first.Bytes(), second.Bytes())
			require.Contains(t, first.String(), `"failure_code":"`+string(cause)+`"`)

			restored := newTestStore(t)
			require.NoError(t, restored.ImportMetadata(t.Context(), bytes.NewReader(first.Bytes())))
			got, err := restored.RenditionJobWaiterByID(t.Context(), mixed.rejectedWaiter.ID)
			require.NoError(t, err)
			require.Equal(t, cause, got.FailureCode)
			publishedWaiter := mixed.fixture.firstWaiter
			if publishedWaiter.ID == mixed.rejectedWaiter.ID {
				publishedWaiter = mixed.fixture.secondWaiter
			}
			got, err = restored.RenditionJobWaiterByID(t.Context(), publishedWaiter.ID)
			require.NoError(t, err)
			require.Equal(t, "published", got.State)
			require.Empty(t, got.FailureCode)
			got, err = restored.RenditionJobWaiterByID(t.Context(), mixed.waitingWaiter.ID)
			require.NoError(t, err)
			require.Equal(t, "waiting", got.State)
			require.Empty(t, got.FailureCode)
			_, _, err = restored.EnqueueRenditionJob(t.Context(), mixed.waitingRequest)
			require.ErrorIs(t, err, ErrProcessingConsentRequired,
				"restored consent remains bound to the old processing incarnation")

			for _, test := range []struct {
				name   string
				mutate func(map[string]jsontext.Value)
			}{
				{name: "absent", mutate: func(fields map[string]jsontext.Value) {
					delete(fields, "failure_code")
				}},
				{name: "null", mutate: func(fields map[string]jsontext.Value) {
					fields["failure_code"] = jsontext.Value("null")
				}},
			} {
				t.Run(test.name, func(t *testing.T) {
					legacy := mutateRenditionWaiterMetadata(
						t, first.Bytes(), "rejected", test.mutate)
					target := newTestStore(t)
					require.NoError(t, target.ImportMetadata(t.Context(), bytes.NewReader(legacy)))
					got, err := target.RenditionJobWaiterByID(t.Context(), mixed.rejectedWaiter.ID)
					require.NoError(t, err)
					require.Equal(t, "rejected", got.State)
					require.Empty(t, got.FailureCode)
				})
			}
		})
	}
}

func TestRenditionWaiterFailureMetadataRejectsInvalidValuesAtomically(t *testing.T) {
	mixed := renditionWaiterMixedMetadataFixture(t, RenditionFailureConsent)
	var exported bytes.Buffer
	require.NoError(t, mixed.fixture.store.ExportMetadata(t.Context(), &exported))

	for _, test := range []struct {
		name   string
		state  string
		mutate func(map[string]jsontext.Value)
		want   string
	}{
		{name: "rejected unknown", state: "rejected", want: "invalid rendition job waiter failure",
			mutate: func(fields map[string]jsontext.Value) {
				fields["failure_code"] = jsontext.Value(`"unknown"`)
			}},
		{name: "rejected empty", state: "rejected", want: "invalid rendition job waiter failure",
			mutate: func(fields map[string]jsontext.Value) {
				fields["failure_code"] = jsontext.Value(`""`)
			}},
		{name: "waiting consent", state: "waiting", want: "invalid rendition job waiter failure",
			mutate: func(fields map[string]jsontext.Value) {
				fields["failure_code"] = jsontext.Value(`"consent"`)
			}},
		{name: "published stale authority", state: "published", want: "invalid rendition job waiter failure",
			mutate: func(fields map[string]jsontext.Value) {
				fields["failure_code"] = jsontext.Value(`"stale_authority"`)
			}},
		{name: "arbitrary optional member", state: "waiting", want: "unknown or non-canonical field",
			mutate: func(fields map[string]jsontext.Value) {
				fields["provider_error"] = jsontext.Value(`"synthetic"`)
			}},
		{name: "required member remains required", state: "waiting", want: "lacks required field",
			mutate: func(fields map[string]jsontext.Value) {
				delete(fields, "attachment_id")
			}},
	} {
		t.Run(test.name, func(t *testing.T) {
			malformed := mutateRenditionWaiterMetadata(
				t, exported.Bytes(), test.state, test.mutate)
			target := newTestStore(t)
			err := target.ImportMetadata(t.Context(), bytes.NewReader(malformed))
			require.ErrorContains(t, err, test.want)
			var waiters, nodes int
			require.NoError(t, target.db.QueryRowContext(t.Context(), `SELECT
				(SELECT COUNT(*) FROM rendition_job_waiters),(SELECT COUNT(*) FROM nodes)`).Scan(
				&waiters, &nodes))
			require.Zero(t, waiters)
			require.Equal(t, 1, nodes)
		})
	}
}

type renditionWaiterMixedMetadata struct {
	fixture        renditionWaiterStatusFixture
	rejectedWaiter RenditionJobWaiter
	waitingRequest RenditionJobRequest
	waitingWaiter  RenditionJobWaiter
}

func renditionWaiterMixedMetadataFixture(
	t *testing.T, cause RenditionFailureCode,
) renditionWaiterMixedMetadata {
	t.Helper()
	fixture := newRenditionWaiterStatusFixture(t)
	now := time.Now().UTC().Add(time.Second)
	claim, err := fixture.store.ClaimRenditionJob(
		t.Context(), fixture.job.ID, "worker:waiter-metadata", now, time.Minute)
	require.NoError(t, err)
	work, err := fixture.store.RenditionJobWorkByClaim(t.Context(), claim, now)
	require.NoError(t, err)
	selectedRequest, _, rejectedRequest, rejectedWaiter := fixture.waitersForSelected(work.Waiter.ID)
	_, err = fixture.store.BeginRenditionProvider(t.Context(), claim, work.Waiter.ID,
		now.Add(time.Second), renditionJobTestSnapshot(selectedRequest))
	require.NoError(t, err)
	invalidateRenditionWaiterAuthority(t, fixture.store, rejectedRequest, cause)
	build := catalogRenditionBuild(fixture.store, fixture.profile)
	build.ID = fixture.job.ID
	require.NoError(t, fixture.store.StageRenditionJobBuild(
		t.Context(), claim, build, now.Add(2*time.Second)))
	_, err = fixture.store.StageRenditionJobGeneration(t.Context(), claim,
		testSHA256([]byte("waiter metadata mixed generation")), now.Add(3*time.Second))
	require.NoError(t, err)
	_, err = fixture.store.PublishRenditionJob(t.Context(), claim, now.Add(4*time.Second))
	require.NoError(t, err)
	rejectedWaiter, err = fixture.store.RenditionJobWaiterByID(t.Context(), rejectedWaiter.ID)
	require.NoError(t, err)
	require.Equal(t, cause, rejectedWaiter.FailureCode)
	if cause == RenditionFailureStaleAuthority {
		_, err = fixture.store.db.ExecContext(t.Context(),
			`UPDATE content_versions SET blob_hash=? WHERE version_id=?`,
			fixture.job.SourceSHA256, rejectedRequest.ContentVersionID)
		require.NoError(t, err)
	}

	third, err := fixture.store.CreateFile(t.Context(), fixture.store.RootID(),
		"synthetic-waiting-source.pdf", catalogSourceHash, 20, "application/pdf")
	require.NoError(t, err)
	waitingRequest := renditionJobTestRequest(third.CurrentVersionID, fixture.profile)
	waitingRequest.Authorization.Principal = "operator:waiter-status-waiting"
	grantRenditionJobConsent(t, fixture.store, waitingRequest)
	_, waitingWaiter, err := fixture.store.EnqueueRenditionJob(t.Context(), waitingRequest)
	require.NoError(t, err)
	waitingWaiter, err = fixture.store.RenditionJobWaiterByID(t.Context(), waitingWaiter.ID)
	require.NoError(t, err)
	require.Equal(t, "waiting", waitingWaiter.State)
	require.Empty(t, waitingWaiter.FailureCode)
	return renditionWaiterMixedMetadata{
		fixture: fixture, rejectedWaiter: rejectedWaiter,
		waitingRequest: waitingRequest, waitingWaiter: waitingWaiter,
	}
}

func mutateRenditionWaiterMetadata(
	t *testing.T, input []byte, state string, mutate func(map[string]jsontext.Value),
) []byte {
	t.Helper()
	lines := bytes.Split(bytes.TrimSpace(input), []byte{'\n'})
	for index, line := range lines {
		var record metadataRenditionJobWaiter
		if err := json.Unmarshal(line, &record); err != nil ||
			record.Type != metadataRenditionJobWaiterType || record.State != state {
			continue
		}
		var fields map[string]jsontext.Value
		require.NoError(t, json.Unmarshal(line, &fields))
		mutate(fields)
		var err error
		lines[index], err = json.Marshal(fields, json.Deterministic(true))
		require.NoError(t, err)
		return append(bytes.Join(lines, []byte{'\n'}), '\n')
	}
	require.FailNow(t, "rendition waiter metadata state not found", state)
	return nil
}
