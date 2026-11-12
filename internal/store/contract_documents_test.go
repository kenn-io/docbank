package store

import (
	"bytes"
	"database/sql"
	"encoding/json/v2"
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func checkContractDocumentNodeHighWater(t *testing.T, open func(*testing.T) *Store) {
	t.Helper()
	ctx := t.Context()
	s := open(t)
	n, err := s.CreateFile(ctx, s.RootID(), "retained.txt", fakeHash("f1"), 2, "text/plain")
	require.NoError(t, err)
	c, err := s.CreateContract(ctx, ContractInput{Title: "Retained"})
	require.NoError(t, err)
	_, err = s.LinkContractDocument(ctx, c.ContractID, c.Revision, n.ID, "signed", "")
	require.NoError(t, err)
	_, _, err = s.Trash(ctx, n.ID, n.Revision)
	require.NoError(t, err)
	_, err = s.TrashEmpty(ctx, 0, true)
	require.NoError(t, err)
	var exported bytes.Buffer
	require.NoError(t, s.ExportMetadata(ctx, &exported))
	valid := newTestStore(t)
	require.NoError(t, valid.ImportMetadata(ctx, bytes.NewReader(exported.Bytes())))
	got, err := valid.GetContract(ctx, c.ContractID, "2024-01-01")
	require.NoError(t, err)
	require.Len(t, got.Documents, 1)
	assert.Equal(t, n.ID, got.Documents[0].NodeID)
	assert.False(t, got.Documents[0].Available)

	lines := strings.Split(exported.String(), "\n")
	var header metadataHeader
	require.NoError(t, json.Unmarshal([]byte(lines[0]), &header))
	require.GreaterOrEqual(t, header.NodeSequence, n.ID)
	changed := false
	for i, line := range lines {
		if strings.Contains(line, `"type":"contract_document"`) {
			old := fmt.Sprintf(`"node_id":%d,`, n.ID)
			require.Contains(t, line, old)
			lines[i] = strings.Replace(line, old, fmt.Sprintf(`"node_id":%d,`, header.NodeSequence+1), 1)
			changed = true
		}
	}
	require.True(t, changed)
	if s.postgresSchema != "" {
		var validationErr error
		rollback := errors.New("rollback corrupted fixture")
		err = s.withStorageTx(ctx, func(tx *sql.Tx) error {
			if _, err := tx.ExecContext(ctx, `DELETE FROM contract_documents WHERE contract_id=$1`, c.ContractID); err != nil {
				return err
			}
			d := got.Documents[0]
			if _, err := tx.ExecContext(ctx, `INSERT INTO contract_documents(contract_id,node_id,content_version_id,blob_sha256,size,role,added_at) VALUES ($1,$2,$3,$4,$5,$6,$7)`, c.ContractID, header.NodeSequence+1, d.ContentVersionID, d.BlobSHA256, d.Size, d.Role, d.AddedAt); err != nil {
				return err
			}
			validationErr = s.validateMetadataState(ctx, tx, header.NodeSequence)
			return rollback
		})
		require.ErrorIs(t, err, rollback)
		require.Error(t, validationErr, "retained node ID must not exceed the allocation high-water mark")
		got, err := s.GetContract(ctx, c.ContractID, "2024-01-01")
		require.NoError(t, err)
		require.Len(t, got.Documents, 1)
		assert.Equal(t, n.ID, got.Documents[0].NodeID)
		return
	}
	target := newTestStore(t)
	err = target.ImportMetadata(ctx, strings.NewReader(strings.Join(lines, "\n")))
	require.Error(t, err, "retained node ID must not exceed the allocation high-water mark")
	_, err = target.GetContract(ctx, c.ContractID, "2024-01-01")
	assert.ErrorIs(t, err, ErrNotFound, "rejected import must leave no contract authority")
}

// checkContractDocumentRevisionExhaustion exercises imported contract authority at the integer limit.
func checkContractDocumentRevisionExhaustion(t *testing.T, postgres bool) {
	t.Helper()
	for _, action := range []string{"link", "unlink", "noop", "stale", "boundary", "delete"} {
		t.Run(action, func(t *testing.T) {
			ctx := t.Context()
			source := newTestStore(t)
			n, err := source.CreateFile(ctx, source.RootID(), "agreement.txt", fakeHash("a1"), 12, "text/plain")
			require.NoError(t, err)
			c, err := source.CreateContract(ctx, ContractInput{Title: "Agreement"})
			require.NoError(t, err)
			c, err = source.LinkContractDocument(ctx, c.ContractID, c.Revision, n.ID, "signed", n.CurrentVersionID)
			require.NoError(t, err)
			var original bytes.Buffer
			require.NoError(t, source.ExportMetadata(ctx, &original))
			lines := strings.Split(original.String(), "\n")
			revision := int64(math.MaxInt64)
			if action == "boundary" {
				revision--
			}
			found := false
			for i, line := range lines {
				if strings.Contains(line, `"type":"contract"`) && strings.Contains(line, c.ContractID) {
					lines[i] = strings.Replace(line, `"revision":2`, `"revision":`+strconv.FormatInt(revision, 10), 1)
					require.NotEqual(t, line, lines[i])
					found = true
				}
			}
			require.True(t, found)
			s := newTestStore(t)
			require.NoError(t, s.ImportMetadata(ctx, strings.NewReader(strings.Join(lines, "\n"))))
			if postgres {
				s, _ = copyRetentionStore(t, s)
			}
			before, err := s.GetContract(ctx, c.ContractID, "2024-01-01")
			require.NoError(t, err)
			require.Equal(t, revision, before.Revision)
			require.Len(t, before.Documents, 1)
			var snapshot bytes.Buffer
			require.NoError(t, s.ExportMetadata(ctx, &snapshot))
			switch action {
			case "link", "boundary":
				_, err = s.LinkContractDocument(ctx, c.ContractID, revision, n.ID, "draft", n.CurrentVersionID)
			case "unlink":
				_, err = s.UnlinkContractDocument(ctx, c.ContractID, revision, n.ID, "signed", n.CurrentVersionID)
			case "noop":
				_, err = s.LinkContractDocument(ctx, c.ContractID, revision, n.ID, "signed", n.CurrentVersionID)
			case "stale":
				_, err = s.LinkContractDocument(ctx, c.ContractID, revision-1, n.ID, "draft", n.CurrentVersionID)
			case "delete":
				err = s.DeleteContract(ctx, c.ContractID, revision)
			}
			switch action {
			case "link", "unlink":
				assert.ErrorIs(t, err, ErrInvalidContract)
			case "stale":
				assert.ErrorIs(t, err, ErrStaleRevision)
			default:
				require.NoError(t, err)
			}
			if action == "delete" {
				_, err = s.GetContract(ctx, c.ContractID, "2024-01-01")
				require.ErrorIs(t, err, ErrNotFound)
				return
			}
			got, getErr := s.GetContract(ctx, c.ContractID, "2024-01-01")
			if assert.NoError(t, getErr) {
				if action == "boundary" {
					assert.Equal(t, int64(math.MaxInt64), got.Revision)
					assert.Len(t, got.Documents, 2)
				} else {
					assert.Equal(t, before, got)
				}
			}
			var after bytes.Buffer
			if assert.NoError(t, s.ExportMetadata(ctx, &after)) && action != "boundary" {
				assert.Equal(t, snapshot.Bytes(), after.Bytes())
			}
		})
	}
}

func checkContractDocumentLifecycle(t *testing.T, open func(*testing.T) *Store) {
	t.Helper()
	s := open(t)
	ctx := t.Context()
	n, err := s.CreateFile(ctx, s.RootID(), "agreement.txt", fakeHash("a1"), 12, "text/plain")
	require.NoError(t, err)
	c, err := s.CreateContract(ctx, ContractInput{Title: "Agreement"})
	require.NoError(t, err)
	c, err = s.LinkContractDocument(ctx, c.ContractID, c.Revision, n.ID, "signed", "")
	require.NoError(t, err)
	require.Len(t, c.Documents, 1)
	assert.True(t, c.Documents[0].Available)
	assert.True(t, c.Documents[0].Current)
	version := c.Documents[0].ContentVersionID
	same, err := s.LinkContractDocument(ctx, c.ContractID, c.Revision, n.ID, "signed", version)
	require.NoError(t, err)
	assert.Equal(t, c.Revision, same.Revision)
	_, err = s.LinkContractDocument(ctx, c.ContractID, c.Revision-1, n.ID, "signed", version)
	require.ErrorIs(t, err, ErrStaleRevision)
	_, err = s.LinkContractDocument(ctx, c.ContractID, c.Revision, n.ID+10000, "signed", version)
	require.ErrorIs(t, err, ErrInvalidContract)
	_, err = s.LinkContractDocument(ctx, c.ContractID, c.Revision, n.ID, "unknown", version)
	require.ErrorIs(t, err, ErrInvalidContract)
	other, err := s.CreateFile(ctx, s.RootID(), "other.txt", fakeHash("b1"), 1, "text/plain")
	require.NoError(t, err)
	_, err = s.LinkContractDocument(ctx, c.ContractID, c.Revision, other.ID, "signed", version)
	require.ErrorIs(t, err, ErrInvalidContract)
	_, err = s.LinkContractDocument(ctx, c.ContractID, c.Revision, s.RootID(), "signed", "")
	require.ErrorIs(t, err, ErrNotFile)
	c, err = s.LinkContractDocument(ctx, c.ContractID, c.Revision, n.ID, "draft", version)
	require.NoError(t, err)
	list, err := s.ListContracts(ctx, ContractListOptions{NodeID: n.ID, AsOf: "2024-01-01", Limit: 10})
	require.NoError(t, err)
	require.Len(t, list, 1)
	require.Len(t, list[0].Documents, 2)
	largeID, err := s.ListContracts(ctx, ContractListOptions{NodeID: 1 << 33, AsOf: "2024-01-01", Limit: 10})
	require.NoError(t, err)
	assert.Empty(t, largeID)
	second, err := s.CreateContract(ctx, ContractInput{Title: "Second agreement"})
	require.NoError(t, err)
	_, err = s.LinkContractDocument(ctx, second.ContractID, second.Revision, n.ID, "signed", version)
	require.NoError(t, err)
	for _, nodeID := range []int64{0, n.ID} {
		page, err := s.ListContracts(ctx, ContractListOptions{NodeID: nodeID, AsOf: "2024-01-01", Limit: 1, Offset: 1})
		require.NoError(t, err)
		require.Len(t, page, 1)
		assert.Equal(t, second.ContractID, page[0].ContractID)
	}
	c, err = s.UnlinkContractDocument(ctx, c.ContractID, c.Revision, n.ID, "draft", "")
	require.NoError(t, err)
	require.Len(t, c.Documents, 1)
	var buf bytes.Buffer
	require.NoError(t, s.ExportMetadata(ctx, &buf))
	dst := newTestStore(t)
	require.NoError(t, dst.ImportMetadata(ctx, bytes.NewReader(buf.Bytes())))
	got, err := dst.GetContract(ctx, c.ContractID, "2024-01-01")
	require.NoError(t, err)
	require.Len(t, got.Documents, 1)
	assert.Equal(t, version, got.Documents[0].ContentVersionID)
}
func checkContractDocumentHistory(t *testing.T, open func(*testing.T) *Store) {
	t.Helper()
	s := open(t)
	ctx := t.Context()
	n, err := s.CreateFile(ctx, s.RootID(), "source.txt", fakeHash("aa"), 2, "text/plain")
	require.NoError(t, err)
	old := n.CurrentVersionID
	c, err := s.CreateContract(ctx, ContractInput{Title: "History"})
	require.NoError(t, err)
	c, err = s.LinkContractDocument(ctx, c.ContractID, c.Revision, n.ID, "signed", old)
	require.NoError(t, err)
	n, _, err = s.ReplaceContent(ctx, n.ID, n.Revision, fakeHash("bb"), 3, "text/plain")
	require.NoError(t, err)
	c, err = s.GetContract(ctx, c.ContractID, "2024-01-01")
	require.NoError(t, err)
	require.True(t, c.Documents[0].Available)
	require.False(t, c.Documents[0].Current)
	c, err = s.LinkContractDocument(ctx, c.ContractID, c.Revision, n.ID, "signed", "")
	require.NoError(t, err)
	_, err = s.UnlinkContractDocument(ctx, c.ContractID, c.Revision, n.ID, "signed", "")
	require.ErrorIs(t, err, ErrAmbiguousContractDocument)
	n, _, err = s.Trash(ctx, n.ID, n.Revision)
	require.NoError(t, err)
	same, err := s.LinkContractDocument(ctx, c.ContractID, c.Revision, n.ID, "signed", old)
	require.NoError(t, err)
	assert.Equal(t, c.Revision, same.Revision)
	c, err = s.GetContract(ctx, c.ContractID, "2024-01-01")
	require.NoError(t, err)
	for _, d := range c.Documents {
		assert.False(t, d.Available)
		assert.False(t, d.Current)
	}
	n, _, err = s.Restore(ctx, n.ID, n.Revision)
	require.NoError(t, err)
	c, err = s.GetContract(ctx, c.ContractID, "2024-01-01")
	require.NoError(t, err)
	for _, d := range c.Documents {
		assert.True(t, d.Available)
	}
	pruned, err := s.PruneContentVersions(ctx, n.ID, n.Revision, VersionPruneSelector{VersionIDs: []string{old}}, true)
	require.NoError(t, err)
	n = pruned.Node
	c, err = s.GetContract(ctx, c.ContractID, "2024-01-01")
	require.NoError(t, err)
	for _, d := range c.Documents {
		if d.ContentVersionID == old {
			assert.False(t, d.Available)
			assert.False(t, d.Current)
		}
	}
	n, _, err = s.Trash(ctx, n.ID, n.Revision)
	require.NoError(t, err)
	_, err = s.TrashEmpty(ctx, 0, true)
	require.NoError(t, err)
	c, err = s.GetContract(ctx, c.ContractID, "2024-01-01")
	require.NoError(t, err)
	for _, d := range c.Documents {
		assert.False(t, d.Available)
		assert.False(t, d.Current)
	}
	list, err := s.ListContracts(ctx, ContractListOptions{NodeID: n.ID, AsOf: "2024-01-01", Limit: 10})
	require.NoError(t, err)
	require.Len(t, list, 1)
	var buf bytes.Buffer
	require.NoError(t, s.ExportMetadata(ctx, &buf))
	target := newTestStore(t)
	require.NoError(t, target.ImportMetadata(ctx, bytes.NewReader(buf.Bytes())))
	restored, err := target.GetContract(ctx, c.ContractID, "2024-01-01")
	require.NoError(t, err)
	require.Len(t, restored.Documents, 2)
	for _, d := range restored.Documents {
		assert.False(t, d.Available)
	}
	c, err = s.UnlinkContractDocument(ctx, c.ContractID, c.Revision, n.ID, "signed", old)
	require.NoError(t, err)
	require.Len(t, c.Documents, 1)
}

// Competing document mutations share the contract revision fence.
func checkContractDocumentConcurrentFence(t *testing.T, open func(*testing.T) *Store) {
	t.Helper()
	s := open(t)
	ctx := t.Context()
	n, err := s.CreateFile(ctx, s.RootID(), "fence.txt", fakeHash("d1"), 1, "text/plain")
	require.NoError(t, err)
	c, err := s.CreateContract(ctx, ContractInput{Title: "Fence"})
	require.NoError(t, err)
	start := make(chan struct{})
	results := make(chan error, 2)
	var wg sync.WaitGroup
	for _, role := range []string{"draft", "signed"} {
		wg.Go(func() {
			<-start
			_, err := s.LinkContractDocument(ctx, c.ContractID, c.Revision, n.ID, role, "")
			results <- err
		})
	}
	close(start)
	wg.Wait()
	close(results)
	success, stale := 0, 0
	for err := range results {
		if err == nil {
			success++
		} else if errors.Is(err, ErrStaleRevision) {
			stale++
		} else {
			t.Fatalf("unexpected error: %v", err)
		}
	}
	assert.Equal(t, 1, success)
	assert.Equal(t, 1, stale)
}

func TestContractDocumentMetadataRejectsInconsistentCopy(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := t.Context()
	n, err := s.CreateFile(ctx, s.RootID(), "metadata.txt", fakeHash("e1"), 2, "text/plain")
	require.NoError(t, err)
	c, err := s.CreateContract(ctx, ContractInput{Title: "Metadata"})
	require.NoError(t, err)
	_, err = s.LinkContractDocument(ctx, c.ContractID, c.Revision, n.ID, "signed", "")
	require.NoError(t, err)
	var buf bytes.Buffer
	require.NoError(t, s.ExportMetadata(ctx, &buf))
	for _, tc := range []struct{ from, to string }{
		{`"role":"signed"`, `"role":"invalid"`},
		{`"size":2,"role":"signed"`, `"size":3,"role":"signed"`},
	} {
		mutated := strings.Replace(buf.String(), tc.from, tc.to, 1)
		require.NotEqual(t, buf.String(), mutated)
		target := newTestStore(t)
		require.Error(t, target.ImportMetadata(ctx, strings.NewReader(mutated)))
	}
}
