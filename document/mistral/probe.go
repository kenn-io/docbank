package mistral

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode"

	"go.kenn.io/docbank/document"
)

// ProbeConfig controls one explicit authenticated capability probe.
type ProbeConfig struct {
	Fixtures   ProbeFixtureConfig
	ObservedAt time.Time
}

// RunCapabilityProbe probes the complete fixture matrix serially. It returns
// sanitized observations only.
func RunCapabilityProbe(
	ctx context.Context,
	client *Client,
	config ProbeConfig,
) (_ CapabilityManifest, err error) {
	if client == nil {
		return CapabilityManifest{}, errors.New("mistral capability probe requires a client")
	}
	if config.ObservedAt.IsZero() {
		config.ObservedAt = time.Now().UTC()
	}
	observedOn := config.ObservedAt.UTC().Format(time.DateOnly)
	observed, parseErr := time.Parse(time.DateOnly, observedOn)
	if parseErr != nil || observed.After(time.Now().UTC().Add(24*time.Hour)) {
		return CapabilityManifest{}, errors.New("mistral capability probe has invalid observation date")
	}
	fixtures, err := loadProbeFixtures(ctx, client.policy, config.Fixtures)
	if err != nil {
		return CapabilityManifest{}, err
	}
	defer func() { err = errors.Join(err, releaseProbeFixtures(fixtures)) }()

	manifest := CapabilityManifest{
		SchemaVersion: CapabilitySchemaVersion, ProbeFixtureContract: probeFixtureContract,
		ObservedOn: observedOn, Endpoint: client.policy.values.Endpoint,
		Region: client.policy.values.Region, RequestedModel: client.policy.values.Model,
		MaxUnits: client.policy.values.MaxUnits,
		Results:  make([]CapabilityResult, 0, len(candidateFormats)),
	}
	for _, candidate := range candidateFormats {
		if err := ctx.Err(); err != nil {
			return CapabilityManifest{}, err
		}
		fixture := fixtures[candidate.ID]
		snapshot, err := fixture.snapshot()
		if err != nil {
			return CapabilityManifest{}, err
		}
		fixtureDigest, err := shortFixtureDigest(snapshot.sha256)
		if err != nil {
			return CapabilityManifest{}, fmt.Errorf("mistral probe fixture %q: %w", candidate.ID, err)
		}
		options := probeRequestOptions(
			candidate, manifest.MaxUnits,
			client.policy.values.ExtractHeader, client.policy.values.ExtractFooter,
		)
		result := CapabilityResult{
			FormatID: candidate.ID, Family: candidate.Family, MediaType: candidate.MediaType,
			UnitKind: candidate.UnitKind, FixtureDigest: fixtureDigest,
			RequestFingerprint: requestFingerprint(candidate, options), UnitBoundMethod: UnitBoundNone,
		}
		response, processErr := client.process(ctx, fixture.snapshot, options, UnitBoundNone, manifest.MaxUnits)
		if processErr != nil {
			if ctxErr := ctx.Err(); ctxErr != nil {
				return CapabilityManifest{}, ctxErr
			}
			result.Status, result.ReasonCode = classifyProbeError(processErr)
			manifest.Results = append(manifest.Results, result)
			continue
		}
		if !sourceDocumentHasText(response.Document) {
			result.Status = ProbeStatusFailed
			result.ReasonCode = "empty_output"
			manifest.Results = append(manifest.Results, result)
			continue
		}
		sentinel, err := ProbeFixtureSentinel(candidate.ID)
		if err != nil {
			return CapabilityManifest{}, err
		}
		if !probeDocumentContains(response.Document, sentinel) {
			result.Status = ProbeStatusFailed
			result.ReasonCode = "sentinel_missing"
			manifest.Results = append(manifest.Results, result)
			continue
		}
		result.Status = ProbeStatusPassed
		result.ReturnedModel = response.ReturnedModel
		result.UnitCount = len(response.Document.Units)
		result.UnitsProcessed = response.UnitsProcessed
		result.ProviderBytes = response.ProviderBytes
		observeUnitBound(ctx, client, fixture, candidate, &result)
		if ctxErr := ctx.Err(); ctxErr != nil {
			return CapabilityManifest{}, ctxErr
		}
		manifest.Results = append(manifest.Results, result)
	}
	if err := manifest.ValidateComplete(); err != nil {
		return CapabilityManifest{}, err
	}
	return manifest, nil
}

func observeUnitBound(
	ctx context.Context,
	client *Client,
	fixture probeFixture,
	candidate CandidateFormat,
	result *CapabilityResult,
) {
	method := expectedUnitBound(candidate.ID)
	switch method {
	case UnitBoundNone:
		return
	case UnitBoundProviderRequest:
		if result.UnitCount <= 1 || result.UnitCount >= client.policy.values.MaxUnits {
			result.ReasonCode = reasonBoundFixtureOutOfRange
			return
		}
		requested := result.UnitCount - 1
		options := requestOptions{
			Pages:         fmt.Sprintf("0-%d", requested-1),
			ExtractHeader: client.policy.values.ExtractHeader,
			ExtractFooter: client.policy.values.ExtractFooter,
		}
		bounded, err := client.process(ctx, fixture.snapshot, options, UnitBoundProviderRequest, requested)
		if err != nil {
			if errors.Is(err, ErrCapabilityContract) {
				result.ReasonCode = reasonBoundUnitsMismatch
			} else {
				result.ReasonCode = reasonBoundRequestFailed
			}
			return
		}
		if bounded.UnitsProcessed != requested || len(bounded.Document.Units) != requested {
			result.ReasonCode = reasonBoundUnitsMismatch
			return
		}
		result.UnitBoundMethod = UnitBoundProviderRequest
		result.FixtureUnits = result.UnitCount
		result.BoundRequestedUnits = requested
		result.BoundUnitsProcessed = bounded.UnitsProcessed
	case UnitBoundLocalExact:
		snapshot, err := fixture.snapshot()
		if err != nil {
			result.ReasonCode = reasonBoundRequestFailed
			return
		}
		if snapshot.localUnits <= 0 || snapshot.localUnits != result.UnitsProcessed {
			result.ReasonCode = reasonBoundUnitsMismatch
			return
		}
		result.UnitBoundMethod = UnitBoundLocalExact
		result.LocalUnits = snapshot.localUnits
	}
}

func classifyProbeError(err error) (ProbeStatus, string) {
	switch {
	case errors.Is(err, ErrPermanentResponse):
		return ProbeStatusRejected, "provider_4xx"
	case errors.Is(err, ErrTransientResponse):
		return ProbeStatusFailed, "transient_exhausted"
	case errors.Is(err, ErrResponseTooLarge):
		return ProbeStatusFailed, "response_too_large"
	default:
		return ProbeStatusFailed, "invalid_or_local_failure"
	}
}

func probeDocumentContains(source document.SourceDocument, sentinel string) bool {
	var text strings.Builder
	for _, unit := range source.Units {
		text.WriteString(unit.Header)
		text.WriteByte(' ')
		text.WriteString(unit.Markdown)
		text.WriteByte(' ')
		text.WriteString(unit.Footer)
		text.WriteByte(' ')
	}
	return strings.Contains(normalizeProbeText(text.String()), normalizeProbeText(sentinel))
}

func normalizeProbeText(value string) string {
	var normalized strings.Builder
	space := true
	for _, character := range strings.ToLower(value) {
		if unicode.IsLetter(character) || unicode.IsNumber(character) {
			normalized.WriteRune(character)
			space = false
			continue
		}
		if !space {
			normalized.WriteByte(' ')
			space = true
		}
	}
	return strings.TrimSpace(normalized.String())
}
