// Package reporting prepares frozen search-term report evidence and publishes
// calculations without consulting mutable vault state after preparation.
package reporting

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json/v2"
	"errors"
	"fmt"
	"slices"

	"go.kenn.io/docbank/report"
)

var (
	ErrReviewRequired         = errors.New("report date review required")
	ErrIncompleteCoverage     = errors.New("report population coverage is incomplete")
	ErrIncompleteDateCoverage = errors.New("report date coverage is incomplete")
)

type TextReader interface {
	ReadCapturedRendition(ctx context.Context, binding report.TextBinding, budget report.Budget) ([]byte, error)
}

type FrameSource interface {
	MaterializeTermReportFrame(ctx context.Context, request report.Request, coverage report.CoverageSelection, budget, textBudget report.Budget) (report.Frame, error)
}

type Service struct {
	Source   FrameSource
	Text     TextReader
	Coverage func(context.Context) (report.CoverageSelection, error)
	Capture  func(context.Context, func() error) error
	Budget   report.Budget
}

// Prepare captures one store observation and derives candidates from only its
// bound metadata and verified text. Full text is discarded before publication.
func (s *Service) Prepare(ctx context.Context, request report.Request) (report.Frame, error) {
	if s == nil || s.Source == nil || s.Budget == nil {
		return report.Frame{}, errors.New("report service is not configured")
	}
	if s.Capture == nil {
		return s.prepareCaptured(ctx, request)
	}
	var frame report.Frame
	err := s.Capture(ctx, func() error {
		var captureErr error
		frame, captureErr = s.prepareCaptured(ctx, request)
		return captureErr
	})
	return frame, err
}

func (s *Service) prepareCaptured(ctx context.Context, request report.Request) (report.Frame, error) {
	request, err := report.NormalizeRequest(request)
	if err != nil {
		return report.Frame{}, err
	}
	requestJSON, err := json.Marshal(request)
	if err != nil {
		return report.Frame{}, errors.Join(report.ErrReportLimit, err)
	}
	if len(requestJSON) > report.MaxRequestSummaryJSONBytes {
		return report.Frame{}, report.ErrReportLimit
	}
	selection := report.CoverageSelection{Configuration: "unconfigured"}
	if s.Coverage != nil {
		selection, err = s.Coverage(ctx)
		if err != nil {
			return report.Frame{}, err
		}
	}
	if selection.ConfigurationSHA256 == "" {
		// The selected profile fingerprint identifies the executable policy;
		// the digest also preserves the explicit unconfigured/required state.
		digest := sha256.Sum256([]byte("docbank-report-coverage/v1\x00" +
			selection.Configuration + "\x00" + selection.ProfileFingerprint))
		selection.ConfigurationSHA256 = hex.EncodeToString(digest[:])
	}
	nativeTextScope := s.Budget.Child()
	defer func() { _ = nativeTextScope.Close() }()
	frame, err := s.Source.MaterializeTermReportFrame(ctx, request, selection, s.Budget, nativeTextScope)
	if err != nil {
		return report.Frame{}, err
	}
	frame = cloneFrame(frame)
	frame.Request = request
	frame.CoverageSelection = selection
	if len(frame.Members) > 50000 || len(frame.Texts) > 50000 || len(frame.RawDateFields) > 200000 {
		return report.Frame{}, fmt.Errorf("%w: frame exceeds evidence limits", report.ErrReportLimit)
	}
	byIdentity := make(map[report.Identity]int, len(frame.Members))
	for i, member := range frame.Members {
		if _, exists := byIdentity[member.Identity]; exists {
			return report.Frame{}, errors.New("duplicate frozen document identity")
		}
		byIdentity[member.Identity] = i
	}
	adapted, err := report.AdaptDateFields(ctx, s.Budget, frame.RawDateFields)
	if err != nil {
		return report.Frame{}, err
	}
	for _, candidate := range adapted {
		index, ok := byIdentity[candidate.Document]
		if !ok {
			return report.Frame{}, errors.New("date field names a document outside the frozen population")
		}
		frame.Members[index].Candidates = append(frame.Members[index].Candidates, candidate)
	}
	var aggregateTextBytes int64
	for i := range frame.Texts {
		if err := ctx.Err(); err != nil {
			return report.Frame{}, err
		}
		binding := &frame.Texts[i]
		index, ok := byIdentity[binding.Document]
		if !ok {
			return report.Frame{}, errors.New("text binding names a document outside the frozen population")
		}
		aggregateTextBytes += binding.Size
		if binding.Size < 0 {
			return report.Frame{}, errors.New("report text has a negative size")
		}
		if binding.Size > 16<<20 || aggregateTextBytes > 512<<20 {
			return report.Frame{}, fmt.Errorf("%w: inspected text exceeds limit", report.ErrReportLimit)
		}
		var bytes []byte
		var textScope report.Budget
		switch binding.Kind {
		case "native":
			if binding.Native == nil {
				return report.Frame{}, errors.New("native text binding has no captured authority")
			}
			bytes = binding.Native.Text
		case "rendition":
			if s.Text == nil {
				return report.Frame{}, errors.New("captured rendition text reader is unavailable")
			}
			textScope = s.Budget.Child()
			bytes, err = s.Text.ReadCapturedRendition(ctx, *binding, textScope)
			if err != nil {
				_ = textScope.Close()
				return report.Frame{}, err
			}
		default:
			return report.Frame{}, fmt.Errorf("unsupported frozen text binding %q", binding.Kind)
		}
		candidates, err := report.ExtractContentDates(ctx, s.Budget, binding.Document, *binding, bytes, "")
		if textScope != nil {
			_ = textScope.Close()
		}
		if err != nil {
			return report.Frame{}, err
		}
		frame.Members[index].Candidates = append(frame.Members[index].Candidates, candidates...)
		if binding.Native != nil {
			binding.Native.Text = nil
		}
	}
	var candidateCount int
	for _, member := range frame.Members {
		candidateCount += len(member.Candidates)
		if len(member.Candidates) > 256 {
			return report.Frame{}, &report.ContentDateLimitError{Document: member.Identity, Limit: 256}
		}
		if candidateCount > 200000 {
			return report.Frame{}, report.ErrReportLimit
		}
	}
	return frame, nil
}

// Finalize applies reviewed choices and all per-row counts to the frozen frame.
// A caller may invoke it again with different choices to create a revision.
func (s *Service) Finalize(ctx context.Context, prepared report.Frame, choices []report.DateChoice) (report.Result, error) {
	if s == nil || s.Budget == nil {
		return report.Result{}, errors.New("report service is not configured")
	}
	frame := cloneFrame(prepared)
	if choices != nil {
		frame.Request.DateChoices = slices.Clone(choices)
	}
	request, err := report.NormalizeRequest(frame.Request)
	if err != nil {
		return report.Result{}, err
	}
	frame.Request = request
	choiceByDocument := make(map[report.Identity]*report.DateChoice, len(request.DateChoices))
	for i := range request.DateChoices {
		choice := &request.DateChoices[i]
		choiceByDocument[choice.Document] = choice
	}
	var reviewRequired bool
	var dateUnknown bool
	for i := range frame.Members {
		if err := ctx.Err(); err != nil {
			return report.Result{}, err
		}
		member := &frame.Members[i]
		choice := choiceByDocument[member.Identity]
		delete(choiceByDocument, member.Identity)
		member.Selection, err = report.SelectDate(member.Kind, member.Candidates, choice, request)
		switch {
		case err == nil:
		case errors.Is(err, report.ErrAmbiguousDate):
			reviewRequired = true
		case errors.Is(err, report.ErrUnusableDate):
			dateUnknown = true
		default:
			return report.Result{}, err
		}
	}
	if len(choiceByDocument) != 0 {
		return report.Result{}, fmt.Errorf("%w: choice targets an absent document", report.ErrStaleChoice)
	}
	if reviewRequired {
		return report.Result{}, ErrReviewRequired
	}
	if dateUnknown && request.CoverageMode == "strict" {
		return report.Result{}, ErrIncompleteDateCoverage
	}
	result, err := report.Calculate(ctx, s.Budget, frame)
	if err != nil {
		return report.Result{}, err
	}
	if request.CoverageMode == "strict" {
		for _, row := range result.Frame.RowCoverage {
			if row.MissingText != 0 || row.IncompleteFamilies != 0 {
				return report.Result{}, ErrIncompleteCoverage
			}
		}
	}
	return result, nil
}

func cloneFrame(frame report.Frame) report.Frame {
	clone := frame
	clone.Members = make([]report.Member, len(frame.Members))
	for i, member := range frame.Members {
		clone.Members[i] = member
		clone.Members[i].Candidates = slices.Clone(member.Candidates)
		clone.Members[i].RawMatches = slices.Clone(member.RawMatches)
		clone.Members[i].Eligible = slices.Clone(member.Eligible)
		clone.Members[i].Hits = slices.Clone(member.Hits)
		clone.Members[i].CollectionWitnesses = slices.Clone(member.CollectionWitnesses)
		clone.Members[i].Coverage.Diagnostics = slices.Clone(member.Coverage.Diagnostics)
	}
	clone.Texts = slices.Clone(frame.Texts)
	for i, binding := range clone.Texts {
		if binding.Native != nil {
			copied := *binding.Native
			clone.Texts[i].Native = &copied
		}
	}
	clone.Relations = slices.Clone(frame.Relations)
	clone.RawDateFields = slices.Clone(frame.RawDateFields)
	clone.Dependencies = slices.Clone(frame.Dependencies)
	clone.Coverage.Warnings = slices.Clone(frame.Coverage.Warnings)
	clone.RowCoverage = slices.Clone(frame.RowCoverage)
	return clone
}
