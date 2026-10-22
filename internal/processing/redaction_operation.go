package processing

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"strings"
)

// RecoverNativePDFSpool removes operations abandoned by a previous process.
// The caller must hold the processing spool lock before recovery and keep it
// until all operations using this directory have stopped.
func RecoverNativePDFSpool(ctx context.Context, directory string) error {
	root, err := os.OpenRoot(directory)
	if err != nil {
		return err
	}
	defer func() { _ = root.Close() }()
	entries, err := fs.ReadDir(root.FS(), ".")
	if err != nil {
		return fmt.Errorf("reading native PDF spool: %w", err)
	}
	for _, entry := range entries {
		if err := ctx.Err(); err != nil {
			return err
		}
		if !entry.IsDir() || !strings.HasPrefix(entry.Name(), "native-pdf-") {
			continue
		}
		if err := root.RemoveAll(entry.Name()); err != nil {
			return fmt.Errorf("removing native PDF staging %s: %w", entry.Name(), err)
		}
	}
	return nil
}

// NativePDFBusyError identifies which operation owns the shared native PDF slot.
type NativePDFBusyError struct{ Operation string }

func (e *NativePDFBusyError) Error() string {
	return fmt.Sprintf("native PDF %s is in progress; retry after it finishes (render may take up to 10 minutes)", e.Operation)
}
func (e *NativePDFBusyError) Unwrap() error { return ErrRedactionPreparationBusy }

type nativePDFOperation struct {
	ctx       context.Context
	directory string
	service   *Service
	finish    func()
}

func (service *Service) beginNativePDFOperation(ctx context.Context, kind string) (*nativePDFOperation, error) {
	runCtx, finish, err := service.beginRun(ctx)
	if err != nil {
		return nil, ErrRedactionPreparationUnavailable
	}
	service.runsMu.Lock()
	if service.lifecycle.Err() != nil {
		service.runsMu.Unlock()
		finish()
		return nil, ErrRedactionPreparationUnavailable
	}
	if holder := service.nativePDFOperation; holder != "" {
		service.runsMu.Unlock()
		finish()
		return nil, &NativePDFBusyError{Operation: holder}
	}
	service.nativePDFOperation = kind
	service.runsMu.Unlock()
	op := &nativePDFOperation{ctx: runCtx, service: service, finish: finish}
	if err := os.MkdirAll(service.spoolDirectory, 0700); err != nil {
		return nil, errors.Join(err, op.Close())
	}
	op.directory, err = os.MkdirTemp(service.spoolDirectory, "native-pdf-*")
	if err != nil {
		return nil, errors.Join(err, op.Close())
	}
	return op, nil
}

func (op *nativePDFOperation) Close() error {
	var err error
	if op.directory != "" {
		err = os.RemoveAll(op.directory)
	}
	op.service.runsMu.Lock()
	op.service.nativePDFOperation = ""
	stopped := op.service.lifecycle.Err() != nil
	op.service.runsMu.Unlock()
	op.finish()
	if stopped {
		err = errors.Join(err, ErrRedactionPreparationUnavailable)
	}
	return err
}
