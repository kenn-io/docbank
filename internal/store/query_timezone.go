package store

import (
	"context"
	"errors"
	"fmt"

	"go.kenn.io/docbank/internal/query"
)

type queryDefaultZoneKey struct{}

// WithQueryDefaultTimezone supplies the validated vault default to every query consumer.
func WithQueryDefaultTimezone(ctx context.Context, zone string) context.Context {
	return context.WithValue(ctx, queryDefaultZoneKey{}, zone)
}

// QueryDefaultZone returns the configured zone or the embedded default.
func QueryDefaultZone(ctx context.Context) string {
	zone, _ := ctx.Value(queryDefaultZoneKey{}).(string)
	if zone == "" {
		return "UTC" // Embedded callers without configured web settings use the documented default.
	}
	return zone
}

// Each saved scope keeps its own zone. The first dated scope supplies the
// receipt echo; its nested predicates still execute with their own zones.
func resolveDocumentDateZones(resolved *query.ResolvedQuery, defaultZone string) (string, bool) {
	var zone string
	var defaulted bool
	if query.UsesDocumentDate(resolved.Query) {
		defaulted = resolved.Query.Timezone == nil
		if defaulted {
			zone := defaultZone
			resolved.Query.Timezone = &zone
		}
		zone = *resolved.Query.Timezone
	}
	var visit func(*query.ResolvedExpression)
	visit = func(e *query.ResolvedExpression) {
		if e == nil {
			return
		}
		if e.Saved != nil {
			z, d := resolveDocumentDateZones(e.Saved, defaultZone)
			if zone == "" && z != "" {
				zone, defaulted = z, d
			}
		}
		for _, child := range e.Children {
			visit(child)
		}
	}
	visit(resolved.Expression)
	return zone, defaulted
}

// currentDocumentDateHead restricts every date read to the successful attempt
// that published this head. Failed attempts deliberately retain older heads.
var currentDocumentDateHead = `EXISTS (SELECT 1 FROM document_event_attempts a
 JOIN document_event_generations g ON g.generation_id=h.generation_id
 JOIN document_event_state s ON s.singleton=1
 LEFT JOIN document_event_dirty d ON d.content_version_id=h.content_version_id
 WHERE a.content_version_id=h.content_version_id AND ` + documentEventFreshIndexedSQL("s.input_epoch", "s.contract_version", "s.deriver_fingerprint") + `)`

// ErrDocumentDatesPending prevents a rebuild from appearing to be a complete empty result.
var ErrDocumentDatesPending = errors.New("document date indexing is pending")

func requireDocumentDateIndex(ctx context.Context, q metadataQuerier) error {
	state, err := effectiveDocumentEventState(ctx, q, DocumentEventsDeriverFingerprint)
	if err != nil {
		return err
	}
	var pending bool
	err = q.QueryRowContext(ctx, `SELECT EXISTS (
		SELECT 1 FROM nodes n JOIN content_versions cv ON cv.version_id=n.current_version_id
		LEFT JOIN document_event_dirty d ON d.content_version_id=cv.version_id
		LEFT JOIN document_event_attempts a ON a.content_version_id=cv.version_id
		LEFT JOIN document_event_heads h ON h.content_version_id=cv.version_id
		WHERE n.kind='file' AND n.trashed_at IS NULL AND NOT COALESCE(
			a.input_epoch=$1 AND a.input_revision=COALESCE(d.revision,0) AND
			(a.state IN ('failed','unavailable') OR `+currentDocumentDateHead+`), FALSE)
	)`, state.InputEpoch).Scan(&pending)
	if err != nil {
		return fmt.Errorf("checking document date index: %w", err)
	}
	if pending {
		return ErrDocumentDatesPending
	}
	return nil
}
