package emailpdf

import (
	"context"
	"errors"
	"github.com/google/uuid"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

const unitMarker = ".docbank-email-pdf-unit"

func stopUnit(ctx context.Context, unit string) error {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	stop := exec.CommandContext(ctx, "sudo", "-n", "systemctl", "stop", unit) //nolint:gosec // Generated or recovery-validated exact UUID unit, never a shell command.
	stop.Env = []string{"PATH=/usr/local/bin:/usr/bin:/bin", "LANG=C.UTF-8"}
	stop.Stdout, stop.Stderr = io.Discard, io.Discard
	stopErr := stop.Run()
	check := exec.CommandContext(ctx, "sudo", "-n", "systemctl", "show", unit, "--property=LoadState", "--value") //nolint:gosec // The same validated exact unit is queried without a shell.
	check.Env = stop.Env
	state, err := check.Output()
	if err != nil || strings.TrimSpace(string(state)) != "not-found" {
		return errors.Join(errors.New("email PDF worker cleanup could not be verified"), stopErr, err)
	}
	return nil
}

// RecoverStale is called only by the exclusive vault owner before blob tmp
// cleanup. It stops and proves absence of every specifically marked unit
// before deleting its abandoned source/profile/output staging.
func RecoverStale(ctx context.Context, spool string) error {
	entries, err := os.ReadDir(spool)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if !strings.HasPrefix(entry.Name(), "email-pdf-") || !entry.IsDir() {
			continue
		}
		dir := filepath.Join(spool, entry.Name())
		marker := filepath.Join(dir, unitMarker)
		info, err := os.Lstat(marker)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() || info.Size() > 128 {
			return errors.New("invalid email PDF recovery marker")
		}
		b, err := os.ReadFile(marker)
		if err != nil {
			return err
		}
		unit := string(b)
		id := strings.TrimSuffix(strings.TrimPrefix(unit, "docbank-email-pdf-"), ".service")
		parsed, err := uuid.Parse(id)
		if err != nil || parsed.String() != id || unit != "docbank-email-pdf-"+id+".service" {
			return errors.New("invalid email PDF recovery unit")
		}
		if err := stopUnit(ctx, unit); err != nil {
			return err
		}
		if err := os.RemoveAll(dir); err != nil {
			return err
		}
	}
	return nil
}
