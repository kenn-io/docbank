//go:build !darwin

package pdfproduction

import (
	"context"
	"errors"
	"fmt"
	"math"
	"os"

	"github.com/shirou/gopsutil/v4/process"
)

func readProcessRSS(ctx context.Context) (uint64, error) {
	pid := os.Getpid()
	if pid < 0 || pid > math.MaxInt32 {
		return 0, errors.New("invalid PDF worker process ID")
	}
	p, err := process.NewProcess(int32(pid))
	if err != nil {
		return 0, fmt.Errorf("open worker memory accounting: %w", err)
	}
	info, err := p.MemoryInfoWithContext(ctx)
	if err != nil {
		return 0, fmt.Errorf("read worker RSS: %w", err)
	}
	return info.RSS, nil
}
