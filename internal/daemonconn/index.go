package daemonconn

import (
	"context"
	"errors"
	"fmt"
	"time"
	"unicode/utf8"

	"go.kenn.io/docbank/internal/apiclient"
	"go.kenn.io/docbank/internal/processing"
)

// IndexStatus returns the daemon's authorization-filtered serving projection
// state. A fresh wait is always bounded by the server's ten-second limit.
func (c *Connection) IndexStatus(
	ctx context.Context, requireFresh bool, wait time.Duration,
) (processing.IndexStatusReport, error) {
	if wait < 0 || wait > 10*time.Second {
		return processing.IndexStatusReport{}, errors.New("index freshness wait must be between zero and ten seconds")
	}
	waitMilliseconds := wait.Milliseconds()
	response, err := c.API().GetIndexStatus(ctx, &apiclient.GetIndexStatusRequestOptions{
		Query: &apiclient.GetIndexStatusQuery{
			RequireFresh: &requireFresh, WaitMs: &waitMilliseconds,
		},
	})
	if err != nil {
		return processing.IndexStatusReport{}, err
	}
	if err := validateIndexStatusReport(*response); err != nil {
		return processing.IndexStatusReport{}, fmt.Errorf("index status response is invalid: %w", err)
	}
	return *response, nil
}

// PlanIndexRepair previews exact targeted work without starting it.
func (c *Connection) PlanIndexRepair(
	ctx context.Context, targets []processing.IndexTarget,
) (processing.IndexRepairPlan, error) {
	if len(targets) > 64 {
		return processing.IndexRepairPlan{}, errors.New("index repair target count exceeds 64")
	}
	for _, target := range targets {
		if !validIndexTarget(target) {
			return processing.IndexRepairPlan{}, errors.New("index repair target is invalid")
		}
	}
	request := processing.IndexRepairPlanRequest{Targets: append([]processing.IndexTarget(nil), targets...)}
	response, err := c.API().PlanIndexRepair(ctx, &apiclient.PlanIndexRepairRequestOptions{Body: &request})
	if err != nil {
		return processing.IndexRepairPlan{}, err
	}
	if !validSHA256Hex(response.Fingerprint) || len(response.Projections) > 64 {
		return processing.IndexRepairPlan{}, errors.New("index repair plan response is invalid")
	}
	return *response, nil
}

// RepairIndex executes one previously previewed target. Provider work remains
// opt-in even when the plan discloses it.
func (c *Connection) RepairIndex(
	ctx context.Context, target processing.IndexTarget, planFingerprint string,
	allowProviderWork bool,
) (processing.IndexRepairReport, error) {
	if !validIndexTarget(target) || !validSHA256Hex(planFingerprint) {
		return processing.IndexRepairReport{}, errors.New("index repair request is invalid")
	}
	request := processing.IndexRepairRequest{
		Targets: []processing.IndexTarget{target}, PlanFingerprint: planFingerprint,
		AllowProviderWork: allowProviderWork,
	}
	response, err := c.API().RepairIndexes(ctx, &apiclient.RepairIndexesRequestOptions{Body: &request})
	if err != nil {
		return processing.IndexRepairReport{}, err
	}
	if response.PlanFingerprint != planFingerprint || len(response.Projections) != 1 ||
		response.Projections[0].Target != target || !validSHA256Hex(response.Projections[0].Generation) {
		return processing.IndexRepairReport{}, errors.New("index repair response is invalid")
	}
	return *response, nil
}

func validIndexTarget(target processing.IndexTarget) bool {
	switch target.Kind {
	case processing.IndexLexical, processing.IndexMetadata, processing.IndexTag,
		processing.IndexMap, processing.IndexVector:
	default:
		return false
	}
	return utf8.ValidString(target.Key) && len(target.Key) <= 256
}

func validateIndexStatusReport(report processing.IndexStatusReport) error {
	if len(report.Projections) > 64 {
		return errors.New("too many index projections")
	}
	for _, projection := range report.Projections {
		if !validIndexTarget(projection.Target) ||
			projection.Coverage.Indexed > projection.Coverage.Expected ||
			projection.Coverage.Unavailable > projection.Coverage.Expected-projection.Coverage.Indexed {
			return errors.New("index projection status is inconsistent")
		}
		switch projection.State {
		case processing.IndexStateQueryable, processing.IndexStateBuilding,
			processing.IndexStateStale, processing.IndexStateFailed, processing.IndexStateDisabled:
		default:
			return errors.New("index projection state is unknown")
		}
		if projection.Generation != "" && !validSHA256Hex(projection.Generation) {
			return errors.New("index generation identity is invalid")
		}
	}
	return nil
}
