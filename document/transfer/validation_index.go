package transfer

import (
	"bufio"
	"container/heap"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
)

const (
	validationRunKeys    = 10_000
	validationMergeFanIn = 32
)

type validationIndexOptions struct {
	runKeys    int
	mergeFanIn int
}

type validationIndex struct {
	directory string
	relations map[string]*diskRelation
	options   validationIndexOptions
	removeAll func(string) error
	closed    bool
}

type relationResult struct {
	Duplicate  bool
	Missing    bool
	Unexpected bool
}

type diskRelation struct {
	definitions *diskKeyRuns
	references  *diskKeyRuns
}

func newValidationIndex() (*validationIndex, error) {
	return newValidationIndexWithOptions(validationIndexOptions{runKeys: validationRunKeys, mergeFanIn: validationMergeFanIn})
}

func newValidationIndexWithOptions(options validationIndexOptions) (*validationIndex, error) {
	if options.runKeys <= 0 || options.mergeFanIn < 2 {
		return nil, errors.New("transfer: invalid validation index bounds")
	}
	directory, err := os.MkdirTemp("", "docbank-transfer-validation-")
	if err != nil {
		return nil, fmt.Errorf("transfer: create validation spool: %w", err)
	}
	return &validationIndex{directory: directory, relations: make(map[string]*diskRelation), options: options, removeAll: os.RemoveAll}, nil
}

func (index *validationIndex) relation(name string) *diskRelation {
	if relation := index.relations[name]; relation != nil {
		return relation
	}
	relation := &diskRelation{
		definitions: &diskKeyRuns{directory: index.directory, prefix: name + "-definitions", runKeys: index.options.runKeys, mergeFanIn: index.options.mergeFanIn},
		references:  &diskKeyRuns{directory: index.directory, prefix: name + "-references", runKeys: index.options.runKeys, mergeFanIn: index.options.mergeFanIn},
	}
	index.relations[name] = relation
	return relation
}

func (index *validationIndex) Close() error {
	if index.closed {
		return nil
	}
	index.closed = true
	if err := index.removeAll(index.directory); err != nil {
		return fmt.Errorf("transfer: remove validation spool: %w", err)
	}
	return nil
}

func (relation *diskRelation) addDefinition(key string) error {
	return relation.definitions.add(key)
}

func (relation *diskRelation) addReference(key string) error {
	return relation.references.add(key)
}

func (relation *diskRelation) validate(ctx context.Context, exact bool) (result relationResult, resultErr error) {
	if err := ctx.Err(); err != nil {
		return relationResult{}, err
	}
	definitions, err := relation.definitions.iterator(ctx)
	if err != nil {
		return relationResult{}, err
	}
	defer func() { resultErr = errors.Join(resultErr, definitions.close()) }()
	references, err := relation.references.iterator(ctx)
	if err != nil {
		return relationResult{}, err
	}
	defer func() { resultErr = errors.Join(resultErr, references.close()) }()

	definitionSet := uniqueKeys{source: definitions}
	referenceSet := uniqueKeys{source: references}
	definition, hasDefinition, err := definitionSet.next()
	if err != nil {
		return relationResult{}, err
	}
	reference, hasReference, err := referenceSet.next()
	if err != nil {
		return relationResult{}, err
	}
	result = relationResult{}
	for hasDefinition || hasReference {
		if err := ctx.Err(); err != nil {
			return relationResult{}, err
		}
		switch {
		case !hasDefinition:
			result.Missing = true
			reference, hasReference, err = referenceSet.next()
		case !hasReference:
			result.Unexpected = result.Unexpected || exact
			definition, hasDefinition, err = definitionSet.next()
		case definition < reference:
			result.Unexpected = result.Unexpected || exact
			definition, hasDefinition, err = definitionSet.next()
		case definition > reference:
			result.Missing = true
			reference, hasReference, err = referenceSet.next()
		default:
			definition, hasDefinition, err = definitionSet.next()
			if err == nil {
				reference, hasReference, err = referenceSet.next()
			}
		}
		if err != nil {
			return relationResult{}, err
		}
	}
	result.Duplicate = definitionSet.duplicate
	return result, nil
}

type diskKeyRuns struct {
	directory  string
	prefix     string
	runKeys    int
	mergeFanIn int
	buffer     []string
	paths      []string
}

func (runs *diskKeyRuns) add(key string) error {
	if len(key) > MaxRecordLineBytes {
		return errors.New("transfer: validation key limit")
	}
	runs.buffer = append(runs.buffer, key)
	if len(runs.buffer) >= runs.runKeys {
		return runs.flush()
	}
	return nil
}

func (runs *diskKeyRuns) flush() error {
	if len(runs.buffer) == 0 {
		return nil
	}
	sort.Strings(runs.buffer)
	file, err := os.CreateTemp(runs.directory, runs.prefix+"-*")
	if err != nil {
		return fmt.Errorf("transfer: create validation run: %w", err)
	}
	path := file.Name()
	buffered := bufio.NewWriter(file)
	for _, key := range runs.buffer {
		var length [4]byte
		binary.BigEndian.PutUint32(length[:], uint32(len(key))) // #nosec G115 -- keys are bounded by 1 MiB.
		if _, err := buffered.Write(length[:]); err != nil {
			_ = file.Close()
			return fmt.Errorf("transfer: write validation run: %w", err)
		}
		if _, err := buffered.WriteString(key); err != nil {
			_ = file.Close()
			return fmt.Errorf("transfer: write validation run: %w", err)
		}
	}
	if err := buffered.Flush(); err != nil {
		_ = file.Close()
		return fmt.Errorf("transfer: flush validation run: %w", err)
	}
	if err := file.Close(); err != nil {
		return fmt.Errorf("transfer: close validation run: %w", err)
	}
	runs.paths = append(runs.paths, path)
	runs.buffer = runs.buffer[:0]
	return nil
}

func (runs *diskKeyRuns) iterator(ctx context.Context) (*mergedKeyIterator, error) {
	if err := runs.flush(); err != nil {
		return nil, err
	}
	if err := runs.compact(ctx); err != nil {
		return nil, err
	}
	return openMergedKeyIterator(ctx, runs.paths)
}

func (runs *diskKeyRuns) compact(ctx context.Context) error {
	for len(runs.paths) > runs.mergeFanIn {
		if err := ctx.Err(); err != nil {
			return err
		}
		oldPaths := runs.paths
		mergedPaths := make([]string, 0, (len(oldPaths)+runs.mergeFanIn-1)/runs.mergeFanIn)
		for start := 0; start < len(oldPaths); start += runs.mergeFanIn {
			end := min(start+runs.mergeFanIn, len(oldPaths))
			path, err := runs.merge(ctx, oldPaths[start:end])
			if err != nil {
				return err
			}
			mergedPaths = append(mergedPaths, path)
		}
		for _, path := range oldPaths {
			if err := os.Remove(filepath.Clean(path)); err != nil {
				return errors.New("transfer: remove validation run")
			}
		}
		runs.paths = mergedPaths
	}
	return nil
}

func (runs *diskKeyRuns) merge(ctx context.Context, paths []string) (path string, resultErr error) {
	iterator, err := openMergedKeyIterator(ctx, paths)
	if err != nil {
		return "", err
	}
	defer func() { resultErr = errors.Join(resultErr, iterator.close()) }()
	file, err := os.CreateTemp(runs.directory, runs.prefix+"-merged-*")
	if err != nil {
		return "", errors.New("transfer: create merged validation run")
	}
	path = file.Name()
	buffered := bufio.NewWriter(file)
	for {
		value, ok, err := iterator.next()
		if err != nil {
			_ = file.Close()
			return "", err
		}
		if !ok {
			break
		}
		if err := writeValidationKey(buffered, value); err != nil {
			_ = file.Close()
			return "", err
		}
	}
	if err := buffered.Flush(); err != nil {
		_ = file.Close()
		return "", errors.New("transfer: flush merged validation run")
	}
	if err := file.Close(); err != nil {
		return "", errors.New("transfer: close merged validation run")
	}
	return path, nil
}

func openMergedKeyIterator(ctx context.Context, paths []string) (*mergedKeyIterator, error) {
	iterator := &mergedKeyIterator{ctx: ctx}
	for _, path := range paths {
		if err := ctx.Err(); err != nil {
			_ = iterator.close()
			return nil, err
		}
		file, err := os.Open(filepath.Clean(path))
		if err != nil {
			_ = iterator.close()
			return nil, errors.New("transfer: open validation run")
		}
		run := &keyRunReader{file: file, reader: bufio.NewReader(file)}
		iterator.runs = append(iterator.runs, run)
		key, err := run.next()
		if errors.Is(err, io.EOF) {
			if closeErr := run.close(); closeErr != nil {
				_ = iterator.close()
				return nil, closeErr
			}
			continue
		}
		if err != nil {
			_ = iterator.close()
			return nil, err
		}
		heap.Push(&iterator.heap, keyHeapItem{key: key, run: len(iterator.runs) - 1})
	}
	return iterator, nil
}

func writeValidationKey(writer io.Writer, key string) error {
	var length [4]byte
	binary.BigEndian.PutUint32(length[:], uint32(len(key))) // #nosec G115 -- keys are bounded by 1 MiB.
	if _, err := writer.Write(length[:]); err != nil {
		return errors.New("transfer: write validation run")
	}
	if _, err := io.WriteString(writer, key); err != nil {
		return errors.New("transfer: write validation run")
	}
	return nil
}

type keyRunReader struct {
	file   *os.File
	reader *bufio.Reader
}

func (run *keyRunReader) close() error {
	if run.file == nil {
		return nil
	}
	err := run.file.Close()
	run.file = nil
	run.reader = nil
	if err != nil {
		return errors.New("transfer: close validation run")
	}
	return nil
}

func (run *keyRunReader) next() (string, error) {
	var length [4]byte
	if _, err := io.ReadFull(run.reader, length[:]); err != nil {
		return "", err
	}
	size := binary.BigEndian.Uint32(length[:])
	if size > MaxRecordLineBytes {
		return "", errors.New("transfer: corrupt validation run")
	}
	value := make([]byte, int(size))
	if _, err := io.ReadFull(run.reader, value); err != nil {
		return "", fmt.Errorf("transfer: read validation run: %w", err)
	}
	return string(value), nil
}

type keyHeapItem struct {
	key string
	run int
}

type keyHeap []keyHeapItem

func (items *keyHeap) Len() int           { return len(*items) }
func (items *keyHeap) Less(i, j int) bool { return (*items)[i].key < (*items)[j].key }
func (items *keyHeap) Swap(i, j int)      { (*items)[i], (*items)[j] = (*items)[j], (*items)[i] }
func (items *keyHeap) Push(value any) {
	item, ok := value.(keyHeapItem)
	if !ok {
		panic("transfer: invalid validation heap item")
	}
	*items = append(*items, item)
}
func (items *keyHeap) Pop() any {
	old := *items
	last := old[len(old)-1]
	*items = old[:len(old)-1]
	return last
}

type mergedKeyIterator struct {
	ctx  context.Context
	runs []*keyRunReader
	heap keyHeap
}

func (iterator *mergedKeyIterator) next() (string, bool, error) {
	if err := iterator.ctx.Err(); err != nil {
		return "", false, err
	}
	if len(iterator.heap) == 0 {
		return "", false, nil
	}
	item, ok := heap.Pop(&iterator.heap).(keyHeapItem)
	if !ok {
		return "", false, errors.New("transfer: invalid validation heap item")
	}
	key, err := iterator.runs[item.run].next()
	if err == nil {
		heap.Push(&iterator.heap, keyHeapItem{key: key, run: item.run})
	} else if errors.Is(err, io.EOF) {
		if closeErr := iterator.runs[item.run].close(); closeErr != nil {
			return "", false, closeErr
		}
	} else {
		return "", false, err
	}
	return item.key, true, nil
}

func (iterator *mergedKeyIterator) close() error {
	var result error
	for _, run := range iterator.runs {
		if err := run.close(); err != nil {
			result = errors.Join(result, err)
		}
	}
	return result
}

type uniqueKeys struct {
	source    *mergedKeyIterator
	previous  string
	hasValue  bool
	duplicate bool
}

func (keys *uniqueKeys) next() (string, bool, error) {
	for {
		if err := keys.source.ctx.Err(); err != nil {
			return "", false, err
		}
		value, ok, err := keys.source.next()
		if err != nil || !ok {
			return "", ok, err
		}
		if keys.hasValue && value == keys.previous {
			keys.duplicate = true
			continue
		}
		keys.previous = value
		keys.hasValue = true
		return value, true, nil
	}
}
