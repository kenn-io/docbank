package report

import (
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
)

// DateRuleV1 identifies the deterministic v1 date-selection policy.
const DateRuleV1 = "report-date/v1"

var numericDatePattern = regexp.MustCompile(`^([0-9]{1,2})/([0-9]{1,2})/([0-9]{4})$`)

var (
	ErrAmbiguousDate = errors.New("report date needs review")
	ErrInvalidChoice = errors.New("invalid reviewed date choice")
	ErrStaleChoice   = errors.New("reviewed date evidence is stale")
	ErrUnusableDate  = errors.New("no usable report date evidence")
)

// AmbiguousDateError identifies equally preferred candidates with different dates.
type AmbiguousDateError struct {
	CandidateIDs []string
}

func (e *AmbiguousDateError) Error() string {
	return fmt.Sprintf("%s: %s", ErrAmbiguousDate, strings.Join(e.CandidateIDs, ", "))
}

func (*AmbiguousDateError) Unwrap() error { return ErrAmbiguousDate }

// SelectDate applies the report-specific creation date policy to one frozen
// document. A reviewed choice may deliberately select a different semantic role.
func SelectDate(kind string, candidates []DateCandidate, choice *DateChoice, request Request) (DateSelection, error) {
	zone, err := time.LoadLocation(request.Timezone)
	if err != nil || request.Timezone == "" {
		return DateSelection{}, fmt.Errorf("invalid report timezone %q", request.Timezone)
	}
	if choice != nil {
		return selectReviewedDate(candidates, *choice, request, zone)
	}
	type winner struct {
		candidate DateCandidate
		date      string
		tier      int
	}
	var winners []winner
	bestTier := 99
	for _, candidate := range candidates {
		tier := automaticDateTier(kind, candidate)
		if tier < 0 || tier > bestTier {
			continue
		}
		date, err := normalizeCandidateDate(candidate, request, zone, "")
		if err != nil {
			continue
		}
		if tier < bestTier {
			bestTier = tier
			winners = winners[:0]
		}
		winners = append(winners, winner{candidate: candidate, date: date, tier: tier})
	}
	if len(winners) == 0 {
		return DateSelection{}, ErrUnusableDate
	}
	slices.SortFunc(winners, func(a, b winner) int { return strings.Compare(a.candidate.ID, b.candidate.ID) })
	for _, candidate := range winners[1:] {
		if candidate.date != winners[0].date {
			ids := make([]string, len(winners))
			for i, item := range winners {
				ids[i] = item.candidate.ID
			}
			return DateSelection{}, &AmbiguousDateError{CandidateIDs: ids}
		}
	}
	selected := winners[0]
	reason := selected.candidate.SourceClass
	if selected.candidate.SourceClass == "vault_observation" {
		reason = "recorded_fallback"
	}
	return DateSelection{
		CandidateID: selected.candidate.ID,
		Date:        selected.date, RuleID: DateRuleV1,
		Reason: reason, Mode: "automatic",
	}, nil
}

func automaticDateTier(kind string, candidate DateCandidate) int {
	switch candidate.SourceClass {
	case "native":
		switch kind {
		case "email", "message":
			if candidate.Role == "sent" {
				return 0
			}
		case "image", "audio_video":
			if candidate.Role == "captured" {
				return 0
			}
		default:
			if candidate.Role == "authored" {
				return 0
			}
			if candidate.Role == "created" {
				return 1
			}
		}
		if candidate.Role == "created" || candidate.Role == "authored" {
			return 4
		}
	case "content":
		if candidate.Role == "document_date" {
			return 2
		}
	case "source_metadata":
		if candidate.Role == "authored" || candidate.Role == "created" {
			return 4
		}
	case "vault_addition":
		if candidate.Role == "imported" {
			return 5
		}
	case "vault_observation":
		if candidate.Role == "vault_recorded" || candidate.Role == "observed" {
			return 6
		}
	}
	return -1
}

func normalizeCandidateDate(candidate DateCandidate, request Request, reportZone *time.Location, reviewedZone string) (string, error) {
	if candidate.Precision == "year" || candidate.Precision == "month" {
		return "", ErrUnusableDate
	}
	if candidate.Rejection == "ambiguous_numeric_date" && request.NumericDateOrder != "" {
		return parseNumericDate(candidate.Raw, request.NumericDateOrder)
	}
	if candidate.Rejection != "" && candidate.Rejection != "timezone_omitted" {
		return "", ErrUnusableDate
	}
	value := candidate.Value
	if value == "" {
		value = candidate.Raw
	}
	if date, err := parseISODate(value); err == nil {
		return date.Format(time.DateOnly), nil
	}
	if stamp, err := time.Parse(time.RFC3339Nano, value); err == nil {
		return stamp.In(reportZone).Format(time.DateOnly), nil
	}
	sourceZone := reviewedZone
	if sourceZone == "" && candidate.Timezone != "" && candidate.Timezone != "omitted" {
		sourceZone = candidate.Timezone
	}
	if sourceZone == "" {
		sourceZone = request.SourceTimezone
	}
	if sourceZone == "" {
		return "", ErrUnusableDate
	}
	location, err := time.LoadLocation(sourceZone)
	if err != nil {
		return "", ErrUnusableDate
	}
	for _, layout := range []string{"2006-01-02T15:04:05.999999999", "2006-01-02T15:04:05", "2006-01-02T15:04", "2006-01-02 15:04:05"} {
		stamp, err := time.ParseInLocation(layout, value, location)
		if err == nil {
			return stamp.In(reportZone).Format(time.DateOnly), nil
		}
	}
	return "", ErrUnusableDate
}

func selectReviewedDate(candidates []DateCandidate, choice DateChoice, request Request, zone *time.Location) (DateSelection, error) {
	if err := validateChoiceShape(choice); err != nil {
		return DateSelection{}, fmt.Errorf("%w: %w", ErrInvalidChoice, err)
	}
	var found *DateCandidate
	for i := range candidates {
		if candidates[i].ID == choice.CandidateID && candidates[i].Document == choice.Document &&
			candidates[i].Locator.EvidenceSHA256 == choice.EvidenceSHA256 {
			found = &candidates[i]
			break
		}
	}
	if found == nil {
		return DateSelection{}, fmt.Errorf("%w: stale or missing candidate binding", ErrStaleChoice)
	}
	date := ""
	switch choice.Action {
	case "select", "reclassify":
		var err error
		date, err = normalizeCandidateDate(*found, request, zone, "")
		if err != nil {
			return DateSelection{}, fmt.Errorf("%w: selected candidate is unusable", ErrInvalidChoice)
		}
	case "interpret":
		if found.Rejection != "ambiguous_numeric_date" && found.Rejection != "timezone_omitted" {
			return DateSelection{}, fmt.Errorf("%w: candidate cannot be interpreted", ErrInvalidChoice)
		}
		if found.Role == "unclassified" && choice.ReviewedRole == "" {
			return DateSelection{}, fmt.Errorf("%w: unclassified date needs a reviewed role", ErrInvalidChoice)
		}
		if !reviewedDateMatchesTokens(*found, choice, request, zone) {
			return DateSelection{}, fmt.Errorf("%w: reviewed date differs from source tokens", ErrInvalidChoice)
		}
		date = choice.ReviewedDate
	}
	return DateSelection{
		CandidateID: found.ID, Date: date, RuleID: DateRuleV1,
		Reason: choice.Reason, Mode: "override",
	}, nil
}

func reviewedDateMatchesTokens(candidate DateCandidate, choice DateChoice, request Request, reportZone *time.Location) bool {
	if candidate.Rejection == "timezone_omitted" {
		date, err := normalizeCandidateDate(candidate, request, reportZone, choice.ReviewedTimezone)
		return err == nil && date == choice.ReviewedDate
	}
	for _, order := range []string{"MDY", "DMY"} {
		if date, err := parseNumericDate(candidate.Raw, order); err == nil && date == choice.ReviewedDate {
			return true
		}
	}
	return false
}

func parseNumericDate(raw, order string) (string, error) {
	match := numericDatePattern.FindStringSubmatch(raw)
	if match == nil || (order != "MDY" && order != "DMY") {
		return "", ErrUnusableDate
	}
	first, _ := strconv.Atoi(match[1])
	second, _ := strconv.Atoi(match[2])
	year, _ := strconv.Atoi(match[3])
	month, day := first, second
	if order == "DMY" {
		month, day = second, first
	}
	text := fmt.Sprintf("%04d-%02d-%02d", year, month, day)
	if _, err := parseISODate(text); err != nil {
		return "", ErrUnusableDate
	}
	return text, nil
}
