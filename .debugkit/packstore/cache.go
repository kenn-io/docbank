package packstore

import (
	"errors"
	"fmt"
	"slices"
	"sync"

	"go.kenn.io/kit/pack"
)

type cachedPackReader struct {
	id      string
	reader  *pack.Reader
	entries map[pack.BlobID]pack.Entry
	leases  int
	retired bool
}

func (s *Store) acquirePackReader(packID string, enforcePolicy bool) (*cachedPackReader, func() error, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	slot, err := s.cachedReaderLocked(packID, enforcePolicy)
	if err != nil {
		return nil, nil, err
	}
	slot.leases++
	var once sync.Once
	var releaseErr error
	release := func() error {
		once.Do(func() { releaseErr = s.releasePackReader(slot) })
		return releaseErr
	}
	return slot, release, nil
}

func (s *Store) cachedReaderLocked(packID string, enforcePolicy bool) (*cachedPackReader, error) {
	if reader, ok := s.packReaders[packID]; ok {
		return reader, nil
	}
	f, err := openNoFollow(s.layout.PackPath(packID), false)
	if err != nil {
		return nil, fmt.Errorf("packstore: open pack %s: %w", packID, markPhysicalSourceNotFound(err))
	}
	readerLimits := pack.ReaderLimits{}
	if enforcePolicy {
		readerLimits.ContainerBytes = uint64(s.limits.PackBytes)
		readerLimits.FooterBytes = uint64(s.limits.FooterBytes)
		readerLimits.Entries = uint64(s.limits.PackEntries)
	}
	reader, err := pack.NewReaderFromFileWithOptions(f, packID, nil, pack.ReaderOptions{Limits: readerLimits})
	if err != nil {
		return nil, fmt.Errorf("packstore: open pack %s: %w", packID, mapPackStreamLimit(err))
	}
	footerEntries := reader.Entries()
	entries := make(map[pack.BlobID]pack.Entry, len(footerEntries))
	for _, entry := range footerEntries {
		if _, duplicate := entries[entry.ID]; duplicate {
			return nil, errors.Join(fmt.Errorf("%w: duplicate blob id %s", pack.ErrCorrupt, entry.ID), reader.Close())
		}
		entries[entry.ID] = entry
	}
	if err := s.addPackSlotLocked(packID); err != nil {
		return nil, errors.Join(err, reader.Close())
	}
	result := &cachedPackReader{id: packID, reader: reader, entries: entries}
	s.packReaders[packID] = result
	return result, nil
}

func (s *Store) validatePackPolicy(slot *cachedPackReader) error {
	metadata := slot.reader.Metadata()
	if metadata.ContainerBytes > uint64(s.limits.PackBytes) {
		return newLimitError(LimitPackContainerBytes, metadata.ContainerBytes, uint64(s.limits.PackBytes))
	}
	if metadata.FooterBytes > uint64(s.limits.FooterBytes) {
		return newLimitError(LimitPackFooterBytes, metadata.FooterBytes, uint64(s.limits.FooterBytes))
	}
	if metadata.EntryCount > uint64(s.limits.PackEntries) {
		return newLimitError(LimitPackEntryCount, metadata.EntryCount, uint64(s.limits.PackEntries))
	}
	return nil
}

func (s *Store) addPackSlotLocked(packID string) error {
	if slices.Contains(s.order, packID) {
		return nil
	}
	if len(s.order) >= s.slots {
		if err := s.retirePackSlotLocked(s.order[0]); err != nil {
			return err
		}
	}
	s.order = append(s.order, packID)
	return nil
}

func (s *Store) retirePackSlotLocked(packID string) error {
	slot, ok := s.packReaders[packID]
	if !ok {
		return nil
	}
	delete(s.packReaders, packID)
	s.order = slices.DeleteFunc(s.order, func(id string) bool { return id == packID })
	slot.retired = true
	if slot.leases == 0 {
		return slot.reader.Close()
	}
	return nil
}

func (s *Store) releasePackReader(slot *cachedPackReader) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if slot.leases == 0 {
		return fmt.Errorf("packstore: pack %s reader lease released twice", slot.id)
	}
	slot.leases--
	if slot.retired && slot.leases == 0 {
		return slot.reader.Close()
	}
	return nil
}

func mapPackStreamLimit(err error) error {
	limit, ok := errors.AsType[*pack.StreamLimitError](err)
	if !ok {
		return err
	}
	var dimension LimitDimension
	switch limit.Dimension {
	case pack.StreamLimitRawBytes:
		dimension = LimitBlobRawBytes
	case pack.StreamLimitStoredBytes:
		dimension = LimitBlobStoredBytes
	case pack.StreamLimitContainerBytes:
		dimension = LimitPackContainerBytes
	case pack.StreamLimitFooterBytes:
		dimension = LimitPackFooterBytes
	case pack.StreamLimitEntryCount:
		dimension = LimitPackEntryCount
	case pack.StreamLimitWindowBytes:
		dimension = LimitBlobWindowBytes
	default:
		return err
	}
	return newLimitError(dimension, limit.Actual, limit.Limit)
}
