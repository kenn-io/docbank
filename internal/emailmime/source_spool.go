package emailmime

import (
	"errors"
	"os"
)

// SourceSpool holds a seekable source in a private, rooted ownership-marked
// directory. RecoverStale handles it after an interrupted owner process.
type SourceSpool struct {
	*os.File

	spool *ownedSpool
}

func NewSourceSpool(parent string) (*SourceSpool, error) {
	spool, err := createSpool(parent)
	if err != nil {
		return nil, err
	}
	file, err := spool.root.OpenFile("source", os.O_CREATE|os.O_EXCL|os.O_RDWR, 0o600)
	if err != nil {
		return nil, errors.Join(err, spool.cleanup())
	}
	return &SourceSpool{File: file, spool: spool}, nil
}

func (s *SourceSpool) Close() error {
	if s == nil || s.spool == nil {
		return nil
	}
	err := errors.Join(s.File.Close(), s.spool.cleanup())
	s.spool = nil
	return err
}
