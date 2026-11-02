package store

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"net/url"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestPhotoSearchRevalidationCompilesAfterPublication(t *testing.T) {
	t.Parallel()
	s, _ := newRenditionCatalogFixture(t)
	node := browsePhotoNode(t, s, "photo.jpg", browseHash("photo"), "image/jpeg")
	profile := catalogProcessingProfile(t, false)
	build := lexicalSearchBuild(s, profile, catalogBuildID, "mercury harbor")
	build.SourceSHA256 = node.BlobHash
	require.NoError(t, s.StageRenditionBuild(t.Context(), build))
	first, err := s.StageLexicalGeneration(t.Context(), fakeHash("91"))
	require.NoError(t, err)
	attachment := RenditionAttachmentRecord{ID: catalogAttachmentFirst, VaultID: s.VaultID(), ContentVersionID: node.CurrentVersionID, BuildID: build.ID, Profile: profile, AttachedAt: "2026-08-22T10:00:00.000000000Z"}
	head := RenditionHeadRecord{ContentVersionID: node.CurrentVersionID, ProcessingProfileFingerprint: profile.Fingerprint, AttachmentID: attachment.ID, PublishedAt: "2026-08-22T10:01:00.000000000Z"}
	require.NoError(t, s.PublishRenditionAndLexicalHeads(t.Context(), attachment, head, first.ID))
	second, err := s.StageLexicalGeneration(t.Context(), fakeHash("92"))
	require.NoError(t, err)
	publisher, err := Open(t.Context(), s.path)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, publisher.Close()) })

	// Gate only BEGIN. Ordinary reads must remain available so compilation
	// outside the transaction would select the generation publication collects.
	started, release := make(chan struct{}), make(chan struct{})
	resume := sync.OnceFunc(func() { close(release) })
	defer resume()
	readDB := s.db
	s.db = sql.OpenDB(photoSearchBeginConnector{
		driver:  readDB.Driver(),
		uri:     (&url.URL{Scheme: "file", Path: "/" + strings.TrimPrefix(filepath.ToSlash(s.path), "/"), RawQuery: "mode=rw"}).String(),
		started: started, release: release,
	})
	t.Cleanup(func() { require.NoError(t, readDB.Close()) })
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	scope := PhotoBrowseRequest{Query: snapshotTestQuery(t, `{}`)}
	lexical := snapshotTestQuery(t, `{"text":"mercury"}`)
	candidates := []SearchCandidateIdentity{{NodeID: node.ID, ContentVersionID: node.CurrentVersionID, Evidence: []SearchEvidenceIdentity{{NodeID: node.ID, ContentVersionID: node.CurrentVersionID, Kind: "node_name", NodeName: node.Name}}}}
	done := make(chan struct {
		result SearchCandidateRevalidation
		err    error
	}, 1)
	finished := make(chan struct{})
	go func() {
		defer close(finished)
		result, err := s.RevalidateSearchCandidates(ctx, candidates, SearchOptions{PhotoScope: &scope, PhotoLexicalQuery: &lexical}, "", "")
		done <- struct {
			result SearchCandidateRevalidation
			err    error
		}{result, err}
	}()
	defer func() {
		cancel()
		resume()
		<-finished
	}()
	select {
	case <-started:
	case <-ctx.Done():
		t.Fatal("revalidation did not reach BEGIN:", ctx.Err())
	}
	require.NoError(t, publisher.PublishRenditionAndLexicalHeads(t.Context(), attachment, head, second.ID))
	var remaining int
	require.NoError(t, publisher.db.QueryRowContext(t.Context(), `SELECT COUNT(*) FROM rendition_lexical_generations WHERE generation_id=?`, first.ID).Scan(&remaining))
	require.Zero(t, remaining)
	resume()
	got := <-done
	require.NoError(t, got.err)
	require.Len(t, got.result.Candidates, 1)
	require.Equal(t, node.ID, got.result.Candidates[0].NodeID)
}

type photoSearchBeginConnector struct {
	driver           driver.Driver
	uri              string
	started, release chan struct{}
}

func (c photoSearchBeginConnector) Driver() driver.Driver { return c.driver }

func (c photoSearchBeginConnector) Connect(context.Context) (driver.Conn, error) {
	conn, err := c.driver.Open(c.uri)
	if err != nil {
		return nil, err
	}
	return photoSearchBeginConn{Conn: conn, started: c.started, release: c.release}, nil
}

type photoSearchBeginConn struct {
	driver.Conn
	started, release chan struct{}
}

func (c photoSearchBeginConn) BeginTx(ctx context.Context, opts driver.TxOptions) (driver.Tx, error) {
	close(c.started)
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-c.release:
		return c.Conn.(driver.ConnBeginTx).BeginTx(ctx, opts)
	}
}
