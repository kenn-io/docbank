package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/danielgtaylor/huma/v2"
	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/internal/store"
)

type versionFactsInput struct {
	VersionID  string `path:"version_id" format:"uuid"`
	Disclosure string `query:"disclosure" enum:"safe,full" default:"safe"`
}

func factOpen(origin string, nodeID int64, versionID string) *FactOpen {
	if origin == "" {
		return nil
	}
	return &FactOpen{Web: fmt.Sprintf("%s/#node=%d&version=%s", strings.TrimRight(origin, "/"), nodeID, versionID)}
}
func factsError(err error) error {
	if errors.Is(err, store.ErrNotFound) {
		return NewError(http.StatusNotFound, "version_not_found", "Content version not found")
	}
	return FromStoreError(err)
}
func registerDocumentFactsRoutes(api huma.API, d Deps) {
	huma.Register(api, huma.Operation{OperationID: "getVersionDates", Method: http.MethodGet, Path: "/api/v1/versions/{version_id}/dates", Summary: "Read a version's date claims"}, func(ctx context.Context, in *versionFactsInput) (*struct{ Body VersionDates }, error) {
		if browserSessionRequest(ctx) && in.Disclosure == "full" {
			return nil, NewError(http.StatusForbidden, "disclosure_forbidden", "Browser sessions may only request safe disclosure")
		}
		f, err := d.Store.DocumentDateFacts(browserPeopleScope(ctx), in.VersionID)
		if err != nil {
			return nil, factsError(err)
		}
		out := versionDatesAPI(f, in.Disclosure, d.Cfg.Web.PublicOrigin)
		return &struct{ Body VersionDates }{Body: out}, nil
	})
	huma.Register(api, huma.Operation{OperationID: "getVersionPeople", Method: http.MethodGet, Path: "/api/v1/versions/{version_id}/people", Summary: "Read a version's people"}, func(ctx context.Context, in *versionFactsInput) (*struct{ Body VersionPeople }, error) {
		if browserSessionRequest(ctx) && in.Disclosure == "full" {
			return nil, NewError(http.StatusForbidden, "disclosure_forbidden", "Browser sessions may only request safe disclosure")
		}
		safe := in.Disclosure != "full"
		f, err := d.Store.DocumentPeopleFacts(browserPeopleScope(ctx), in.VersionID, safe)
		if err != nil {
			return nil, factsError(err)
		}
		out := versionPeopleAPI(f, safe, d.Cfg.Web.PublicOrigin)
		return &struct{ Body VersionPeople }{Body: out}, nil
	})
}

func versionDatesAPI(f store.DocumentDateFacts, disclosure, origin string) VersionDates {
	out := VersionDates{ContentVersionID: f.Version.ID, NodeID: f.Version.NodeID, State: f.State, Claims: []VersionDateClaim{}, Diagnostics: []FactDiagnostic{}, Open: factOpen(origin, f.Version.NodeID, f.Version.ID)}
	if f.State == "published" {
		out.DocumentKind = f.View.Events.DocumentKind
		for _, p := range f.View.Events.Primaries {
			if p.ScopeClass == "vault" && p.Disclosure == disclosure {
				out.Primary = &VersionDatePrimary{EventID: p.EventID, Reason: p.Reason, RuleID: p.RuleID, Disclosure: p.Disclosure}
				break
			}
		}
		for _, e := range f.View.Events.Events {
			if disclosure != "full" && document.EventHasSensitiveEvidence(e) {
				continue
			}
			out.Claims = append(out.Claims, VersionDateClaim{EventID: e.EventID, DateKind: e.DateKind, Primary: out.Primary != nil && out.Primary.EventID == e.EventID, DateValue: e.DateValue, RawValue: e.RawValue, Precision: e.Precision, TimezoneKind: e.TimezoneKind, ZoneText: e.ZoneText, OffsetSeconds: e.OffsetSeconds, AxisKey: e.AxisKey, UTCKey: e.UTCKey, ClaimBasis: e.ClaimBasis, ParseConfidence: e.ParseConfidence, EvidenceKind: e.EvidenceKind, EvidenceID: e.EvidenceID, SourceKindRaw: e.SourceKindRaw, When: whenFromClaim(e)})
		}
		// Diagnostics can contain source-specific values; only the public code and detail are projected.
		for _, diag := range f.View.Events.Diagnostics {
			out.Diagnostics = append(out.Diagnostics, FactDiagnostic{Code: diag.Code, Detail: diag.Detail})
		}
	}
	return out
}

func versionPeopleAPI(f store.DocumentPeopleFacts, safe bool, origin string) VersionPeople {
	out := VersionPeople{ContentVersionID: f.Version.ID, NodeID: f.Version.NodeID, State: f.State, Edges: []VersionPersonEdge{}, Assertions: []VersionPersonAssertion{}, Unresolved: []VersionUnresolvedName{}, Open: factOpen(origin, f.Version.NodeID, f.Version.ID)}
	for _, e := range f.Edges {
		p, ok := f.Persons[e.PersonID]
		if !ok || safe && (e.Sensitive || e.Role == "blind_copy" || !p.Visible) {
			continue
		}
		edge := VersionPersonEdge{PersonID: e.PersonID, DisplayName: p.Person.DisplayName, Role: e.Role, Basis: e.Basis, Confidence: e.Confidence, EvidenceKind: e.EvidenceKind, EvidenceID: e.EvidenceID, RawLabel: e.RawLabel, ClaimCount: e.ClaimCount, External: []FactExternal{}}
		for _, x := range p.External {
			edge.External = append(edge.External, FactExternal{System: x.System, ArchiveID: x.ArchiveID, UID: x.UID})
		}
		edge.LinkedContact = len(edge.External) > 0
		out.Edges = append(out.Edges, edge)
	}
	for _, a := range f.Assertions {
		if p, ok := f.Persons[a.PersonID]; safe && (!ok || !p.Visible) {
			continue
		}
		out.Assertions = append(out.Assertions, VersionPersonAssertion{AssertionID: a.AssertionID, PersonID: a.PersonID, Role: a.Role, Action: a.Action, Note: a.Note, RecordedAt: a.RecordedAt, Revision: a.Revision})
	}
	for _, c := range f.Candidates {
		for _, o := range c.Occurrences {
			if safe && o.Role == "blind_copy" {
				continue
			}
			out.Unresolved = append(out.Unresolved, VersionUnresolvedName{RawLabel: c.DisplayName, Role: o.Role, CandidateID: c.CandidateID, EvidenceKind: o.EvidenceKind})
		}
	}
	return out
}
