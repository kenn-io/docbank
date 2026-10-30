package ingest

import (
	"io/fs"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func nativeWatchIdentity(t *testing.T, name string) (watchFileIdentity, fs.FileInfo) {
	t.Helper()
	file, err := os.Open(name)
	require.NoError(t, err)
	defer func() { require.NoError(t, file.Close()) }()
	info, err := file.Stat()
	require.NoError(t, err)
	_, identity, err := watchMetadataForFile(file, info)
	require.NoError(t, err)
	return identity, info
}

// requireRenameEvidence skips rename-continuity tests on filesystems without
// usable native birth evidence, where watches keep renamed sources separate.
func requireRenameEvidence(t *testing.T, name string) {
	t.Helper()
	if identity, _ := nativeWatchIdentity(t, name); !matchableWatchIdentity(identity) {
		t.Skip("filesystem did not provide usable native birth evidence")
	}
}

func TestWatchNativeIdentityRenameAndCopy(t *testing.T) {
	dir := t.TempDir()
	original := filepath.Join(dir, "draft.md")
	renamed := filepath.Join(dir, "final.md")
	copyPath := filepath.Join(dir, "copy.md")
	require.NoError(t, os.WriteFile(original, []byte("same"), 0o600))
	before, beforeInfo := nativeWatchIdentity(t, original)
	if !matchableWatchIdentity(before) {
		t.Skip("filesystem did not provide usable native birth evidence")
	}
	require.NoError(t, os.Rename(original, renamed))
	after, afterInfo := nativeWatchIdentity(t, renamed)
	require.Equal(t, before, after)
	assert.True(t, os.SameFile(beforeInfo, afterInfo))
	require.NoError(t, os.WriteFile(copyPath, []byte("same"), 0o600))
	copied, copiedInfo := nativeWatchIdentity(t, copyPath)
	assert.NotEqual(t, before.object, copied.object)
	assert.False(t, os.SameFile(beforeInfo, copiedInfo))
}

func TestWatchRenameMatcherAmbiguities(t *testing.T) {
	dir := t.TempDir()
	one := filepath.Join(dir, "one")
	two := filepath.Join(dir, "two")
	require.NoError(t, os.WriteFile(one, []byte("same"), 0o600))
	require.NoError(t, os.WriteFile(two, []byte("same"), 0o600))
	oneInfo, err := os.Stat(one)
	require.NoError(t, err)
	twoInfo, err := os.Stat(two)
	require.NoError(t, err)
	birth := validWatchBirth(100, 200)
	identity := watchFileIdentity{object: watchObjectKey{mount: 1, file: 7}, birth: birth}
	prior := watchObservation{accepted: &watchAcceptedSource{nodeID: 8, identity: identity, info: oneInfo}}
	for _, tc := range []struct {
		name              string
		current           watchFileIdentity
		info              fs.FileInfo
		oldPresent        bool
		alias             bool
		bound             bool
		aliasMissingBirth bool
		want              bool
	}{
		{"unique", identity, oneInfo, false, false, false, false, true},
		{"hardlink alias", identity, oneInfo, false, true, false, false, false},
		{"changed birth reused object", watchFileIdentity{object: identity.object, birth: validWatchBirth(101, 0)}, oneInfo, false, false, false, false, false},
		{"birth absent", watchFileIdentity{object: identity.object}, oneInfo, false, false, false, false, false},
		{"missing inode response", watchFileIdentity{object: watchObjectKey{mount: 1}, birth: birth}, oneInfo, false, false, false, false, false},
		{"occupied old path", identity, oneInfo, true, false, false, false, false},
		{"destination already bound", identity, oneInfo, false, false, true, false, false},
		{"accepted alias without birth", identity, oneInfo, false, false, false, true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			old := map[string]watchObservation{"draft.md": prior}
			if tc.aliasMissingBirth {
				old["alias.md"] = watchObservation{accepted: &watchAcceptedSource{
					nodeID: 10, identity: watchFileIdentity{object: identity.object}, info: oneInfo,
				}}
			}
			if tc.bound {
				boundIdentity := watchFileIdentity{
					object: watchObjectKey{mount: 1, file: 8}, birth: validWatchBirth(101, 0),
				}
				old["final.md"] = watchObservation{accepted: &watchAcceptedSource{nodeID: 9, identity: boundIdentity, info: twoInfo}}
			}
			inv := &watchInventory{files: map[string]watchInventoryEntry{
				"final.md": {state: watchFileState{identity: tc.current, info: tc.info}},
			}, present: map[string]struct{}{"final.md": {}}, complete: true}
			if tc.oldPresent {
				inv.present["draft.md"] = struct{}{}
			}
			if tc.alias {
				// Two new names for the accepted object leave no unique rename.
				inv.files["alias.md"] = watchInventoryEntry{state: watchFileState{identity: identity, info: oneInfo}}
				inv.present["alias.md"] = struct{}{}
			}
			matches := matchWatchRenames(old, inv)
			assert.Equal(t, tc.want, len(matches) == 1)
		})
	}
	// An atomic replacement seen but not accepted cannot lend its identity to
	// the node last accepted at that path.
	replacement := watchFileIdentity{object: watchObjectKey{mount: 1, file: 9}, birth: birth}
	inv := &watchInventory{files: map[string]watchInventoryEntry{
		"final.md": {state: watchFileState{identity: replacement, info: twoInfo}},
	}, present: map[string]struct{}{"final.md": {}}, complete: true}
	assert.Empty(t, matchWatchRenames(map[string]watchObservation{"draft.md": prior}, inv))
}

func FuzzWatchRenameMatcherUniqueEvidence(f *testing.F) {
	f.Add([]byte{1, 1, 1, 0})
	f.Add([]byte{2, 1, 1, 1})
	f.Add([]byte{7, 100, 7, 100, 7, 0, 0, 0})
	f.Fuzz(func(t *testing.T, data []byte) {
		if len(data) < 4 || len(data) > 32 {
			t.Skip()
		}
		dir := t.TempDir()
		path := filepath.Join(dir, "file")
		require.NoError(t, os.WriteFile(path, []byte("x"), 0o600))
		info, err := os.Stat(path)
		require.NoError(t, err)
		old := make(map[string]watchObservation)
		inv := &watchInventory{files: make(map[string]watchInventoryEntry), present: make(map[string]struct{}), complete: true}
		for i := 0; i+3 < len(data); i += 4 {
			refOld := string(rune('a'+i/4)) + ".md"
			refNew := string(rune('A'+i/4)) + ".md"
			oldID := watchFileIdentity{object: watchObjectKey{mount: 1, file: uint64(data[i])}, birth: validWatchBirth(int64(data[i+1]), 0)}
			newID := watchFileIdentity{object: watchObjectKey{mount: 1, file: uint64(data[i+2])}, birth: validWatchBirth(int64(data[i+3]), 0)}
			old[refOld] = watchObservation{accepted: &watchAcceptedSource{nodeID: int64(i + 1), identity: oldID, info: info}}
			inv.files[refNew] = watchInventoryEntry{state: watchFileState{identity: newID, info: info}}
			inv.present[refNew] = struct{}{}
			if data[i]&1 == 0 {
				inv.present[refOld] = struct{}{}
			}
		}
		usedOld, usedNew := make(map[string]bool), make(map[string]bool)
		for _, pair := range matchWatchRenames(old, inv) {
			require.False(t, usedOld[pair.oldRef])
			require.False(t, usedNew[pair.newRef])
			usedOld[pair.oldRef], usedNew[pair.newRef] = true, true
			prior := old[pair.oldRef].accepted
			count := 0
			for _, observation := range old {
				if observation.accepted != nil && observation.accepted.identity.object == prior.identity.object {
					count++
				}
			}
			require.Equal(t, 1, count, "accepted aliases must count even without birth")
			current := inv.files[pair.newRef].state
			require.True(t, matchableWatchIdentity(prior.identity))
			require.Equal(t, prior.identity, current.identity)
			require.True(t, os.SameFile(prior.info, current.info))
			_, occupied := inv.present[pair.oldRef]
			require.False(t, occupied)
			_, bound := old[pair.newRef]
			require.False(t, bound)
		}
	})
}

func TestWatchExactOldRefAbsence(t *testing.T) {
	source := t.TempDir()
	old := filepath.Join(source, "draft.md")
	moved := filepath.Join(source, "final.md")
	require.NoError(t, os.WriteFile(old, []byte("first"), 0o600))
	pinned, err := os.OpenRoot(source)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, pinned.Close()) })
	info, err := os.Lstat(source)
	require.NoError(t, err)
	root := &watchRoot{path: source, root: pinned, identity: info}
	require.False(t, watchOldRefAbsent(root, "draft.md"), "an exact leaf occupies its old name")
	require.NoError(t, os.Rename(old, moved))
	require.True(t, watchOldRefAbsent(root, "draft.md"))
	require.NoError(t, os.Link(moved, old))
	require.False(t, watchOldRefAbsent(root, "draft.md"), "an exact hard link still occupies the name")
	require.NoError(t, os.Remove(old))
	require.NoError(t, os.WriteFile(old, []byte("different"), 0o600))
	require.False(t, watchOldRefAbsent(root, "draft.md"), "a recreated old name is occupied")
	require.NoError(t, os.Remove(old))

	parent := filepath.Join(source, "notes")
	require.True(t, watchOldRefAbsent(root, "notes/a.md"), "missing exact parent proves absence")
	require.NoError(t, os.Mkdir(parent, 0o700))
	require.True(t, watchOldRefAbsent(root, "notes/a.md"), "missing exact descendant proves absence")
	require.NoError(t, os.WriteFile(filepath.Join(parent, "a.md"), []byte("x"), 0o600))
	require.False(t, watchOldRefAbsent(root, "notes/a.md"))
	require.NoError(t, os.RemoveAll(parent))
	require.NoError(t, os.Symlink(t.TempDir(), parent))
	require.False(t, watchOldRefAbsent(root, "notes/a.md"), "exact symlink parent is uncertain")
	require.NoError(t, os.Remove(parent))
	require.NoError(t, os.WriteFile(parent, []byte("not a directory"), 0o600))
	require.False(t, watchOldRefAbsent(root, "notes/a.md"), "exact nondirectory parent is uncertain")
}

func TestWatchExactOldRefCaseSensitiveHardLinkOccupancy(t *testing.T) {
	source := t.TempDir()
	lower := filepath.Join(source, "a.md")
	upper := filepath.Join(source, "A.md")
	require.NoError(t, os.WriteFile(lower, []byte("first"), 0o600))
	if _, err := os.Stat(upper); err == nil {
		t.Skip("filesystem is case insensitive; distinct spellings cannot coexist")
	} else {
		require.ErrorIs(t, err, fs.ErrNotExist)
	}
	require.NoError(t, os.Link(lower, upper))
	pinned, err := os.OpenRoot(source)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, pinned.Close()) })
	info, err := os.Lstat(source)
	require.NoError(t, err)
	root := &watchRoot{path: source, root: pinned, identity: info}
	require.False(t, watchOldRefAbsent(root, "a.md"), "exact old entry blocks despite matching inode")
	require.False(t, watchOldRefAbsent(root, "A.md"))
	require.NoError(t, os.Remove(lower))
	require.True(t, watchOldRefAbsent(root, "a.md"), "remaining upper entry is a distinct name")
	require.False(t, watchOldRefAbsent(root, "A.md"))
}

func TestWatchPossibleRenameTargetsRequireRebindableEvidence(t *testing.T) {
	file := filepath.Join(t.TempDir(), "file")
	require.NoError(t, os.WriteFile(file, []byte("same"), 0o600))
	info, err := os.Stat(file)
	require.NoError(t, err)
	object := watchObjectKey{mount: 1, file: 7}
	identity := watchFileIdentity{object: object, birth: validWatchBirth(100, 200)}
	entry := func(id watchFileIdentity) watchInventoryEntry {
		return watchInventoryEntry{state: watchFileState{identity: id, info: info}}
	}
	for _, tc := range []struct {
		name     string
		accepted watchFileIdentity
		files    map[string]watchInventoryEntry
		present  []string
		want     bool
	}{
		{"rebindable rename", identity, map[string]watchInventoryEntry{"renamed.md": entry(identity)}, nil, true},
		{"new path without birth", identity, map[string]watchInventoryEntry{"renamed.md": entry(watchFileIdentity{object: object})}, nil, false},
		{"reused object, other birth", identity, map[string]watchInventoryEntry{"renamed.md": entry(watchFileIdentity{object: object, birth: validWatchBirth(101, 0)})}, nil, false},
		{"accepted source without birth", watchFileIdentity{object: object}, map[string]watchInventoryEntry{"renamed.md": entry(watchFileIdentity{object: object})}, nil, false},
		{"old path occupied", identity, map[string]watchInventoryEntry{"renamed.md": entry(identity)}, []string{"draft.md"}, false},
		{"two new names", identity, map[string]watchInventoryEntry{"renamed.md": entry(identity), "alias.md": entry(identity)}, nil, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// draft.md is accepted but not observed as a regular file, as if an
			// unrelated subtree kept the inventory incomplete.
			accepted := map[string]watchObservation{
				"draft.md": {accepted: &watchAcceptedSource{nodeID: 8, identity: tc.accepted, info: info}},
			}
			inventory := &watchInventory{files: tc.files, present: make(map[string]struct{})}
			for ref := range tc.files {
				inventory.present[ref] = struct{}{}
			}
			for _, ref := range tc.present {
				inventory.present[ref] = struct{}{}
			}
			_, deferred := possibleRenameTargets(accepted, inventory)["renamed.md"]
			assert.Equal(t, tc.want, deferred, "only paths a complete inventory could rebind wait")
		})
	}
}
