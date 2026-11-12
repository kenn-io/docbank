package store

import (
	"bytes"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

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
func TestContractDocumentLifecycle(t *testing.T) {
	t.Parallel()
	checkContractDocumentLifecycle(t, newTestStore)
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
	for _, d := range same.Documents {
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
	n, _, err = s.Trash(ctx, n.ID, n.Revision)
	require.NoError(t, err)
	_, err = s.TrashEmpty(ctx, 0, true)
	require.NoError(t, err)
	same, err = s.LinkContractDocument(ctx, c.ContractID, c.Revision, n.ID, "signed", old)
	require.NoError(t, err)
	for _, d := range same.Documents {
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
func TestContractDocumentHistory(t *testing.T) {
	t.Parallel()
	checkContractDocumentHistory(t, newTestStore)
}

func TestContractDocumentPrune(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := t.Context()
	n, err := s.CreateFile(ctx, s.RootID(), "prune.txt", fakeHash("c1"), 1, "text/plain")
	require.NoError(t, err)
	c, err := s.CreateContract(ctx, ContractInput{Title: "Prune"})
	require.NoError(t, err)
	c, err = s.LinkContractDocument(ctx, c.ContractID, c.Revision, n.ID, "signed", "")
	require.NoError(t, err)
	old := n.CurrentVersionID
	n, _, err = s.ReplaceContent(ctx, n.ID, n.Revision, fakeHash("c2"), 2, "text/plain")
	require.NoError(t, err)
	_, err = s.PruneContentVersions(ctx, n.ID, n.Revision, VersionPruneSelector{VersionIDs: []string{old}}, true)
	require.NoError(t, err)
	c, err = s.GetContract(ctx, c.ContractID, "2024-01-01")
	require.NoError(t, err)
	require.Len(t, c.Documents, 1)
	assert.False(t, c.Documents[0].Available)
	assert.False(t, c.Documents[0].Current)
}
