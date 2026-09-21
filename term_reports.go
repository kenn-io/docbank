package docbank

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"io"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"go.kenn.io/docbank/internal/canonical"
	"go.kenn.io/docbank/internal/reporting"
	"go.kenn.io/docbank/report"
)

// PreparedTermReport owns one frozen observation. Dates remains available after
// an initial successful Finalize so callers can review a different choice.
type PreparedTermReport interface {
	Dates(ctx context.Context, page report.DatePageRequest) (report.DatePage, error)
	Finalize(ctx context.Context, choices []report.DateChoice) (TermReportArtifact, error)
	Close() error
}

// TermReportArtifact owns immutable CSV and verification-packet bytes.
type TermReportArtifact interface {
	Summary() report.Summary
	OpenCSV(ctx context.Context) (io.ReadCloser, error)
	OpenBundle(ctx context.Context) (io.ReadCloser, error)
	Close() error
}

type preparedTermReport struct {
	vault  *Vault
	frame  report.Frame
	scope  report.Budget
	closed bool
	pins   int
}

type termReportArtifact struct {
	vault   *Vault
	scope   report.Budget
	summary report.Summary
	csv     []byte
	bundle  []byte
	closed  bool
	pins    int
}

type termReportReader struct {
	vault    *Vault
	artifact *termReportArtifact
	reader   *bytes.Reader
	ctx      context.Context
	cancel   context.CancelFunc
	mu       sync.Mutex
	closed   bool
}

func (v *Vault) reportRootBudget() report.Budget {
	v.reportMu.Lock()
	defer v.reportMu.Unlock()
	if v.reportBudget == nil {
		v.reportBudget = report.NewBudget(1 << 30)
		v.reportPreparations = make(map[*preparedTermReport]struct{})
		v.reportArtifacts = make(map[*termReportArtifact]struct{})
		v.reportReaders = make(map[*termReportReader]struct{})
	}
	return v.reportBudget
}

func (v *Vault) termReportCoverage(request report.Request) (report.CoverageSelection, error) {
	profile := request.Profile
	if profile == "" && len(v.reportProfiles) == 1 {
		for name := range v.reportProfiles {
			profile = name
		}
	}
	if profile == "" {
		if len(v.reportProfiles) == 0 {
			return report.CoverageSelection{Configuration: "unconfigured"}, nil
		}
		return report.CoverageSelection{Configuration: "profile_required"}, nil
	}
	fingerprint, ok := v.reportProfiles[profile]
	if !ok {
		return report.CoverageSelection{}, fmt.Errorf("unknown report processing profile %q", profile)
	}
	return report.CoverageSelection{Configuration: "configured", ProfileFingerprint: fingerprint}, nil
}

func (v *Vault) captureTermReport(ctx context.Context, fn func() error) error {
	for !v.preservation.TryRLock() {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(10 * time.Millisecond):
		}
	}
	defer v.preservation.RUnlock()
	return fn()
}

// PrepareTermReport captures one independently owned embedded vault. The
// caller must Close the preparation and every artifact it finalizes.
func (v *Vault) PrepareTermReport(ctx context.Context, request report.Request) (PreparedTermReport, error) {
	if err := v.begin(); err != nil {
		return nil, err
	}
	defer v.lifecycle.RUnlock()
	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	selection, err := v.termReportCoverage(request)
	if err != nil {
		return nil, err
	}
	scope := v.reportRootBudget().Child()
	service := reporting.Service{Source: v.metadata,
		Text:     reporting.CapturedTextReader{Open: v.blobs.OpenStreamContext},
		Coverage: func(context.Context) (report.CoverageSelection, error) { return selection, nil },
		Capture:  v.captureTermReport, Budget: scope}
	frame, err := service.Prepare(ctx, request)
	if err != nil {
		_ = scope.Close()
		return nil, err
	}
	prepared := &preparedTermReport{vault: v, frame: frame, scope: scope}
	v.reportMu.Lock()
	v.reportPreparations[prepared] = struct{}{}
	v.reportMu.Unlock()
	return prepared, nil
}

func (p *preparedTermReport) Dates(ctx context.Context, page report.DatePageRequest) (report.DatePage, error) {
	if err := p.vault.begin(); err != nil {
		return report.DatePage{}, err
	}
	defer p.vault.lifecycle.RUnlock()
	p.vault.reportMu.Lock()
	closed := p.closed
	p.vault.reportMu.Unlock()
	if closed {
		return report.DatePage{}, ErrClosed
	}
	if page.Limit == 0 {
		page.Limit = 50
	}
	if page.Limit < 1 || page.Limit > 100 {
		return report.DatePage{}, reporting.ErrReportLimit
	}
	memberAt, candidateAt := 0, 0
	if page.Cursor != "" {
		raw, err := base64.RawURLEncoding.DecodeString(page.Cursor)
		if err != nil {
			return report.DatePage{}, reporting.ErrUnavailable
		}
		parts := strings.Split(string(raw), ":")
		if len(parts) != 2 {
			return report.DatePage{}, reporting.ErrUnavailable
		}
		memberAt, err = strconv.Atoi(parts[0])
		if err != nil {
			return report.DatePage{}, reporting.ErrUnavailable
		}
		candidateAt, err = strconv.Atoi(parts[1])
		if err != nil {
			return report.DatePage{}, reporting.ErrUnavailable
		}
	}
	if memberAt < 0 || memberAt > len(p.frame.Members) || candidateAt < 0 {
		return report.DatePage{}, reporting.ErrUnavailable
	}
	result := report.DatePage{Members: make([]report.DateReviewMember, 0, page.Limit)}
	count := 0
	for memberAt < len(p.frame.Members) && len(result.Members) < page.Limit && count < 1000 {
		if err := ctx.Err(); err != nil {
			return report.DatePage{}, err
		}
		member := p.frame.Members[memberAt]
		if candidateAt > len(member.Candidates) {
			return report.DatePage{}, reporting.ErrUnavailable
		}
		remaining := min(len(member.Candidates)-candidateAt, 1000-count)
		item := report.DateReviewMember{Document: member.Identity,
			Candidates:         slices.Clone(member.Candidates[candidateAt : candidateAt+remaining]),
			CandidatesComplete: candidateAt+remaining == len(member.Candidates)}
		for i := range p.frame.Request.DateChoices {
			choice := p.frame.Request.DateChoices[i]
			if choice.Document == member.Identity {
				item.Choice = &choice
			}
		}
		item.Selection, _ = report.SelectDate(member.Kind, member.Candidates, item.Choice, p.frame.Request)
		result.Members = append(result.Members, item)
		for {
			_, exceeded, err := canonical.BoundedSize(result, 1<<20)
			if err != nil {
				return report.DatePage{}, err
			}
			if !exceeded {
				break
			}
			if len(item.Candidates) == 0 {
				return report.DatePage{}, reporting.ErrReportLimit
			}
			item.Candidates = item.Candidates[:len(item.Candidates)-1]
			item.CandidatesComplete = false
			result.Members[len(result.Members)-1] = item
		}
		consumed := len(item.Candidates)
		count += consumed
		candidateAt += consumed
		if candidateAt == len(member.Candidates) {
			memberAt++
			candidateAt = 0
		} else if consumed == 0 {
			return report.DatePage{}, reporting.ErrReportLimit
		}
	}
	if memberAt < len(p.frame.Members) {
		result.NextCursor = base64.RawURLEncoding.EncodeToString([]byte(fmt.Sprintf("%d:%d", memberAt, candidateAt)))
	}
	return result, nil
}

func (p *preparedTermReport) Finalize(ctx context.Context, choices []report.DateChoice) (TermReportArtifact, error) {
	if err := p.vault.begin(); err != nil {
		return nil, err
	}
	defer p.vault.lifecycle.RUnlock()
	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	p.vault.reportMu.Lock()
	if p.closed {
		p.vault.reportMu.Unlock()
		return nil, ErrClosed
	}
	p.pins++
	p.vault.reportMu.Unlock()
	defer p.releasePin()
	scope := p.vault.reportRootBudget().Child()
	service := reporting.Service{Budget: scope}
	result, err := service.Finalize(ctx, p.frame, choices)
	if err != nil {
		_ = scope.Close()
		return nil, err
	}
	var csv bytes.Buffer
	if err := report.WriteCSV(ctx, &csv, result); err != nil {
		_ = scope.Close()
		return nil, err
	}
	if _, err := scope.Reserve(ctx, int64(csv.Len())); err != nil {
		_ = scope.Close()
		return nil, err
	}
	bundle, err := report.BuildBundle(ctx, scope, result)
	if err != nil {
		_ = scope.Close()
		return nil, err
	}
	var random [24]byte
	if _, err := rand.Read(random[:]); err != nil {
		_ = scope.Close()
		return nil, fmt.Errorf("generating report identity: %w", err)
	}
	artifact := &termReportArtifact{vault: p.vault, scope: scope, csv: csv.Bytes(), bundle: bundle,
		summary: report.Summary{ID: hex.EncodeToString(random[:]), State: "complete",
			ObservedAt: p.frame.ObservedAt, Terms: slices.Clone(result.Frame.Request.Terms),
			Counts: slices.Clone(result.Counts), Coverage: result.Frame.Coverage,
			RowCoverage: slices.Clone(result.Frame.RowCoverage), CSVBytes: int64(csv.Len()),
			BundleBytes: int64(len(bundle)), CSVSHA256: termReportDigest(csv.Bytes()),
			BundleSHA256: termReportDigest(bundle)}}
	p.vault.reportMu.Lock()
	if p.closed {
		p.vault.reportMu.Unlock()
		_ = scope.Close()
		return nil, ErrClosed
	}
	p.vault.reportArtifacts[artifact] = struct{}{}
	p.vault.reportMu.Unlock()
	return artifact, nil
}

func termReportDigest(data []byte) string {
	digest := sha256.Sum256(data)
	return hex.EncodeToString(digest[:])
}

func (p *preparedTermReport) releasePin() {
	p.vault.reportMu.Lock()
	defer p.vault.reportMu.Unlock()
	p.pins--
	if p.closed && p.pins == 0 {
		_ = p.scope.Close()
	}
}

func (p *preparedTermReport) Close() error {
	p.vault.reportMu.Lock()
	defer p.vault.reportMu.Unlock()
	if p.closed {
		return nil
	}
	p.closed = true
	delete(p.vault.reportPreparations, p)
	if p.pins == 0 {
		return p.scope.Close()
	}
	return nil
}

func (a *termReportArtifact) Summary() report.Summary {
	summary := a.summary
	summary.Terms = slices.Clone(summary.Terms)
	summary.Counts = slices.Clone(summary.Counts)
	summary.RowCoverage = slices.Clone(summary.RowCoverage)
	summary.Coverage.Warnings = slices.Clone(summary.Coverage.Warnings)
	return summary
}

func (a *termReportArtifact) OpenCSV(ctx context.Context) (io.ReadCloser, error) {
	return a.open(ctx, a.csv)
}

func (a *termReportArtifact) OpenBundle(ctx context.Context) (io.ReadCloser, error) {
	return a.open(ctx, a.bundle)
}

func (a *termReportArtifact) open(ctx context.Context, raw []byte) (io.ReadCloser, error) {
	if err := a.vault.begin(); err != nil {
		return nil, err
	}
	defer a.vault.lifecycle.RUnlock()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	a.vault.reportMu.Lock()
	defer a.vault.reportMu.Unlock()
	if a.closed {
		return nil, ErrClosed
	}
	readerCtx, cancel := context.WithCancel(ctx)
	reader := &termReportReader{vault: a.vault, artifact: a, reader: bytes.NewReader(raw),
		ctx: readerCtx, cancel: cancel}
	a.pins++
	a.vault.reportReaders[reader] = struct{}{}
	return reader, nil
}

func (a *termReportArtifact) Close() error {
	a.vault.reportMu.Lock()
	defer a.vault.reportMu.Unlock()
	if a.closed {
		return nil
	}
	a.closed = true
	delete(a.vault.reportArtifacts, a)
	if a.pins == 0 {
		return a.scope.Close()
	}
	return nil
}

func (r *termReportReader) Read(out []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return 0, ErrClosed
	}
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	n, err := r.reader.Read(out)
	if err != nil && err != io.EOF {
		return n, fmt.Errorf("reading report bytes: %w", err)
	}
	if err == io.EOF {
		return n, io.EOF
	}
	return n, nil
}

func (r *termReportReader) Close() error {
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return nil
	}
	r.closed = true
	r.cancel()
	r.mu.Unlock()
	r.vault.reportMu.Lock()
	defer r.vault.reportMu.Unlock()
	delete(r.vault.reportReaders, r)
	r.artifact.pins--
	if r.artifact.closed && r.artifact.pins == 0 {
		return r.artifact.scope.Close()
	}
	return nil
}

func (v *Vault) closeTermReports() {
	v.reportMu.Lock()
	readers := make([]*termReportReader, 0, len(v.reportReaders))
	for reader := range v.reportReaders {
		readers = append(readers, reader)
	}
	v.reportMu.Unlock()
	for _, reader := range readers {
		_ = reader.Close()
	}
	v.reportMu.Lock()
	defer v.reportMu.Unlock()
	for p := range v.reportPreparations {
		p.closed = true
		_ = p.scope.Close()
		delete(v.reportPreparations, p)
	}
	for a := range v.reportArtifacts {
		a.closed = true
		_ = a.scope.Close()
		delete(v.reportArtifacts, a)
	}
	if v.reportBudget != nil {
		_ = v.reportBudget.Close()
	}
}

var _ PreparedTermReport = (*preparedTermReport)(nil)
var _ TermReportArtifact = (*termReportArtifact)(nil)
var _ io.ReadCloser = (*termReportReader)(nil)
