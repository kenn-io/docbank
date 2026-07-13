package main

import (
	"fmt"
	"io"
	"math"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"golang.org/x/term"

	"go.kenn.io/docbank/internal/api"
)

const (
	backupProgressInterval = 2 * time.Second
	backupProgressTick     = time.Second
	backupProgressBarWidth = 28
)

type backupProgressMode uint8

const (
	backupProgressAuto backupProgressMode = iota
	backupProgressBar
	backupProgressPlain
)

type backupProgressRenderer struct {
	mu         sync.Mutex
	out        io.Writer
	mode       backupProgressMode
	interval   time.Duration
	stage      string
	stageStart time.Time
	lastRender time.Time
	lineOpen   bool
	lastWidth  int
	lastEvent  api.BackupProgress
	tick       time.Duration
	tickerStop chan struct{}
}

func newBackupProgressRenderer(out io.Writer, mode backupProgressMode) *backupProgressRenderer {
	return &backupProgressRenderer{
		out: out, mode: mode, interval: backupProgressInterval, tick: backupProgressTick,
	}
}

func backupProgressModeFromFlag(value string) (backupProgressMode, error) {
	switch value {
	case "", "auto":
		return backupProgressAuto, nil
	case "bar":
		return backupProgressBar, nil
	case "plain":
		return backupProgressPlain, nil
	default:
		return backupProgressAuto, fmt.Errorf(
			"backup create: invalid --progress value %q (want auto, bar, or plain)", value)
	}
}

func (r *backupProgressRenderer) handle(event api.BackupProgress) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if event.Stage != r.stage {
		r.stopTickerLocked()
		if r.lineOpen {
			_, _ = fmt.Fprintln(r.writer())
		}
		r.stage = event.Stage
		r.stageStart = time.Now()
		r.lastRender = time.Time{}
		r.lineOpen = false
		r.lastWidth = 0
	}
	if !event.Final {
		r.lastEvent = event
	}
	if !event.Final && !r.lastRender.IsZero() && time.Since(r.lastRender) < r.interval {
		return
	}
	r.lastRender = time.Now()
	r.render(event)
	if event.Final {
		r.stopTickerLocked()
	} else if r.lineOpen {
		r.startTickerLocked()
	}
}

func (r *backupProgressRenderer) finish() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.stopTickerLocked()
	if r.lineOpen {
		_, _ = fmt.Fprintln(r.writer())
		r.lineOpen = false
		r.lastWidth = 0
	}
}

func (r *backupProgressRenderer) startTickerLocked() {
	if r.tickerStop != nil {
		return
	}
	stop := make(chan struct{})
	r.tickerStop = stop
	tick := r.tick
	interval := r.interval
	go func() {
		ticker := time.NewTicker(tick)
		defer ticker.Stop()
		for {
			select {
			case <-stop:
				return
			case <-ticker.C:
				r.mu.Lock()
				if r.lineOpen && time.Since(r.lastRender) >= interval {
					r.lastRender = time.Now()
					r.render(r.lastEvent)
				}
				r.mu.Unlock()
			}
		}
	}()
}

func (r *backupProgressRenderer) stopTickerLocked() {
	if r.tickerStop == nil {
		return
	}
	close(r.tickerStop)
	r.tickerStop = nil
}

func (r *backupProgressRenderer) render(event api.BackupProgress) {
	pct := backupProgressPercent(event.Done, event.Total)
	counts := strconv.FormatInt(event.Done, 10)
	if event.Total > 0 {
		counts += "/" + strconv.FormatInt(event.Total, 10)
	}
	detail := counts
	if event.BytesDone > 0 || event.BytesTotal > 0 {
		detail += "  " + formatBackupBytes(event.BytesDone)
		if event.BytesTotal > 0 {
			detail += "/" + formatBackupBytes(event.BytesTotal)
		}
		if rate := r.byteRate(event); rate > 0 {
			detail += " @ " + formatBackupBytes(rate) + "/s"
		}
	}
	elapsed := time.Since(r.stageStart).Truncate(time.Second)
	if elapsed >= time.Second {
		detail += "  " + elapsed.String()
	}

	if r.outputMode() == backupProgressPlain {
		done := ""
		if event.Final {
			done = " (done)"
		}
		_, _ = fmt.Fprintf(r.writer(), "%s: %s (%3.0f%%)%s\n",
			event.Stage, detail, pct, done)
		return
	}

	line := fmt.Sprintf("  %-11s %s %3.0f%%  %s",
		event.Stage, backupProgressBarString(pct), pct, detail)
	width := utf8.RuneCountInString(line)
	if width < r.lastWidth {
		line += strings.Repeat(" ", r.lastWidth-width)
	} else {
		r.lastWidth = width
	}
	if event.Final {
		_, _ = fmt.Fprintln(r.writer(), "\r"+line)
		r.lineOpen = false
		r.lastWidth = 0
		return
	}
	_, _ = fmt.Fprint(r.writer(), "\r"+line)
	r.lineOpen = true
}

func (r *backupProgressRenderer) writer() io.Writer {
	if r.out == nil {
		return os.Stderr
	}
	return r.out
}

func (r *backupProgressRenderer) outputMode() backupProgressMode {
	if r.mode != backupProgressAuto {
		return r.mode
	}
	r.mode = backupProgressPlain
	if file, ok := r.writer().(*os.File); ok && term.IsTerminal(int(file.Fd())) {
		r.mode = backupProgressBar
	}
	return r.mode
}

func (r *backupProgressRenderer) byteRate(event api.BackupProgress) int64 {
	elapsed := time.Since(r.stageStart).Seconds()
	if elapsed < 1 || event.BytesDone <= 0 {
		return 0
	}
	return int64(float64(event.BytesDone) / elapsed)
}

func backupProgressPercent(done, total int64) float64 {
	if total <= 0 {
		return 0
	}
	return min(100, max(0, float64(done)/float64(total)*100))
}

func backupProgressBarString(percent float64) string {
	filled := int(math.Round(percent / 100 * backupProgressBarWidth))
	filled = min(backupProgressBarWidth, max(0, filled))
	return "[" + strings.Repeat("█", filled) +
		strings.Repeat("░", backupProgressBarWidth-filled) + "]"
}

func formatBackupBytes(n int64) string {
	if n < 1024 {
		return fmt.Sprintf("%d B", n)
	}
	value := float64(n)
	units := []string{"KiB", "MiB", "GiB", "TiB"}
	for _, unit := range units {
		value /= 1024
		if value < 1024 || unit == units[len(units)-1] {
			return fmt.Sprintf("%.1f %s", value, unit)
		}
	}
	return fmt.Sprintf("%d B", n)
}
