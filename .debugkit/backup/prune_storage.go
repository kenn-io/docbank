package backup

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"runtime"
	"strings"

	"go.kenn.io/kit/pack"
)

type pruneDirs struct{ roots map[string]*os.Root }

func openPruneDirs(r *Repo) (*pruneDirs, error) {
	dirs := &pruneDirs{roots: map[string]*os.Root{}}
	for _, name := range []string{snapshotsDirName, indexesDirName, packsDirName, stagingDirName} {
		root, err := r.openRepositoryDir(name)
		if err != nil {
			return nil, errors.Join(err, dirs.close())
		}
		dirs.roots[name] = root
	}
	return dirs, nil
}

func (d *pruneDirs) close() error {
	var err error
	for _, root := range d.roots {
		err = errors.Join(err, root.Close())
	}
	return err
}

type prunePlan struct {
	result     PruneResult
	indexNames []string
	packSizes  map[string]int64
}

func planPrune(ctx context.Context, dirs *pruneDirs, ext string, live map[pack.BlobID]IndexEntry) (*prunePlan, error) {
	p := &prunePlan{packSizes: map[string]int64{}}
	indexes, err := fs.ReadDir(dirs.roots[indexesDirName].FS(), ".")
	if err != nil {
		return nil, err
	}
	for _, entry := range indexes {
		if !strings.HasSuffix(entry.Name(), indexExt) {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			return nil, err
		}
		if !info.Mode().IsRegular() {
			return nil, fmt.Errorf("backup: index %s is not a regular file", entry.Name())
		}
		p.indexNames = append(p.indexNames, entry.Name())
	}
	byPack := map[string]map[pack.BlobID]IndexEntry{}
	for id, entry := range live {
		if byPack[entry.PackID] == nil {
			byPack[entry.PackID] = map[pack.BlobID]IndexEntry{}
		}
		byPack[entry.PackID][id] = entry
	}
	root := dirs.roots[packsDirName]
	err = fs.WalkDir(root.FS(), ".", func(name string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("backup: refusing symlink in packs: %s", name)
		}
		if info.IsDir() {
			return nil
		}
		if !strings.HasSuffix(name, ext) {
			return nil
		}
		id := strings.TrimSuffix(path.Base(name), ext)
		if !info.Mode().IsRegular() || !pack.IsValidPackID(id) || name != path.Join(id[:2], id+ext) {
			return fmt.Errorf("backup: invalid pack file %s", name)
		}
		p.packSizes[id] = info.Size()
		refs := byPack[id]
		if len(refs) > 0 {
			stored, liveStored, err := prunePackUsage(root, name, id, refs)
			if err != nil {
				return err
			}
			if liveStored >= stored-liveStored {
				return nil
			}
			p.result.PacksToRepack = append(p.result.PacksToRepack, id)
			p.result.LiveBytesToRewrite += liveStored
		}
		p.result.PacksToRemove = append(p.result.PacksToRemove, id)
		p.result.BytesToRemove += info.Size()
		return nil
	})
	if err != nil {
		return nil, err
	}
	for id := range byPack {
		if _, ok := p.packSizes[id]; !ok {
			return nil, fmt.Errorf("backup: live pack %s missing from inventory", id)
		}
	}
	return p, nil
}

func prunePackUsage(root *os.Root, name, id string, live map[pack.BlobID]IndexEntry) (_ uint64, _ uint64, retErr error) {
	f, err := root.Open(name)
	if err != nil {
		return 0, 0, err
	}
	reader, err := pack.NewReaderFromFile(f, id, nil)
	if err != nil {
		return 0, 0, err
	}
	defer func() { retErr = errors.Join(retErr, reader.Close()) }()
	var stored, liveStored uint64
	for _, entry := range reader.Entries() {
		stored += entry.StoredLen
		if _, ok := live[entry.ID]; ok {
			liveStored += entry.StoredLen
		}
	}
	return stored, liveStored, nil
}

func (d *pruneDirs) retireIndexes(ctx context.Context, names []string, progress *progressEmitter) (retErr error) {
	root := d.roots[indexesDirName]
	dir, err := root.Open(".")
	if err != nil {
		return err
	}
	defer func() { retErr = errors.Join(retErr, dir.Close()) }()
	for i, name := range names {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := root.Remove(name); err != nil {
			return err
		}
		if err := syncPruneDir(dir); err != nil {
			return err
		}
		progress.emit(ProgressEvent{Stage: ProgressStagePruneIndexes, Done: int64(i + 1), Total: int64(len(names)), Final: i+1 == len(names)})
	}
	return ctx.Err()
}

func (d *pruneDirs) removePacks(ctx context.Context, p *prunePlan, ext string, progress *progressEmitter) error {
	for i, id := range p.result.PacksToRemove {
		if err := ctx.Err(); err != nil {
			return err
		}
		// Open and retain the shard too, so removal and its durability sync
		// refer to the same directory rather than resolving a later pathname.
		shard, err := d.roots[packsDirName].OpenRoot(id[:2])
		if err != nil {
			return err
		}
		err = removePrunePack(shard, id+ext, func() {
			p.result.RemovedPacks = append(p.result.RemovedPacks, id)
			p.result.BytesRemoved += p.packSizes[id]
		})
		if err := errors.Join(err, shard.Close()); err != nil {
			return err
		}
		progress.emit(ProgressEvent{Stage: ProgressStagePrunePacks, Done: int64(i + 1), Total: int64(len(p.result.PacksToRemove)), BytesDone: p.result.BytesRemoved, BytesTotal: p.result.BytesToRemove, Final: i+1 == len(p.result.PacksToRemove)})
	}
	return ctx.Err()
}

func removePrunePack(root *os.Root, name string, removed func()) (retErr error) {
	dir, err := root.Open(".")
	if err != nil {
		return err
	}
	defer func() { retErr = errors.Join(retErr, dir.Close()) }()
	if err := root.Remove(name); err != nil {
		return err
	}
	removed()
	return syncPruneDir(dir)
}

func syncPruneDir(dir *os.File) error {
	// Match pack.SyncDir's platform contract while syncing the held directory.
	if runtime.GOOS == "windows" {
		return nil
	}
	return dir.Sync()
}
