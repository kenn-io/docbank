package api

import (
	"context"
	"net/http"
	"strings"

	"github.com/danielgtaylor/huma/v2"
	"github.com/danielgtaylor/huma/v2/adapters/humago"
	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/internal/store"
)

type versionFactsInput struct {
	VersionID  string `path:"version_id" format:"uuid"`
	Disclosure string `query:"disclosure" enum:"safe,full" default:"safe"`
}

func versionFactsPath(path string) (string, bool) {
	rest, ok := strings.CutPrefix(path, "/api/v1/versions/")
	if !ok {
		return "", false
	}
	id, kind, ok := strings.Cut(rest, "/")
	return id, ok && (kind == "dates" || kind == "people")
}

func registerDocumentFactsRoutes(api huma.API, d Deps) {
	huma.Register(api, huma.Operation{OperationID: "getVersionDates", Method: http.MethodGet, Path: "/api/v1/versions/{version_id}/dates", Middlewares: huma.Middlewares{rejectFactsUnknownQuery}, Summary: "Read a version's date claims"}, func(ctx context.Context, in *versionFactsInput) (*struct{ Body VersionDates }, error) {
		if browserSessionRequest(ctx) && in.Disclosure == "full" {
			return nil, NewError(http.StatusForbidden, "disclosure_forbidden", "Browser sessions may only request safe disclosure")
		}
		f, err := d.Store.DocumentDateFacts(browserPeopleScope(ctx), in.VersionID)
		if err != nil {
			return nil, FromStoreError(err)
		}
		return &struct{ Body VersionDates }{Body: versionDatesAPI(f, in.Disclosure)}, nil
	})
	huma.Register(api, huma.Operation{OperationID: "getVersionPeople", Method: http.MethodGet, Path: "/api/v1/versions/{version_id}/people", Middlewares: huma.Middlewares{rejectFactsUnknownQuery}, Summary: "Read a version's people"}, func(ctx context.Context, in *versionFactsInput) (*struct{ Body VersionPeople }, error) {
		if browserSessionRequest(ctx) && in.Disclosure == "full" {
			return nil, NewError(http.StatusForbidden, "disclosure_forbidden", "Browser sessions may only request safe disclosure")
		}
		safe := in.Disclosure != "full"
		f, err := d.Store.DocumentPeopleFacts(browserPeopleScope(ctx), in.VersionID, safe)
		if err != nil {
			return nil, FromStoreError(err)
		}
		return &struct{ Body VersionPeople }{Body: versionPeopleAPI(f, safe)}, nil
	})
}

func versionDatesAPI(f store.DocumentDateFacts, disclosure string) VersionDates {
	out := VersionDates{ContentVersionID: f.Version.ID, NodeID: f.Version.NodeID, State: f.State, Claims: []VersionDateClaim{}, Diagnostics: []FactDiagnostic{}}
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
			primary := out.Primary != nil && out.Primary.EventID == e.EventID
			when := whenFromClaim(e)
			if primary {
				when.PlacedBy = &WhenPlacedBy{Source: e.DateKind, Rule: out.Primary.RuleID, Fallback: document.IsFallbackDateKind(e.DateKind), EventID: e.EventID, ClaimBasis: e.ClaimBasis}
			}
			out.Claims = append(out.Claims, VersionDateClaim{EventID: e.EventID, DateKind: e.DateKind, Primary: primary, DateValue: e.DateValue, RawValue: e.RawValue, Precision: e.Precision, TimezoneKind: e.TimezoneKind, ZoneText: e.ZoneText, OffsetSeconds: e.OffsetSeconds, AxisKey: e.AxisKey, UTCKey: e.UTCKey, ClaimBasis: e.ClaimBasis, ParseConfidence: e.ParseConfidence, EvidenceKind: e.EvidenceKind, EvidenceID: e.EvidenceID, SourceKindRaw: e.SourceKindRaw, When: when})
		}
		// Diagnostics can contain source-specific values; only the public code and detail are projected.
		for _, diag := range f.View.Events.Diagnostics {
			out.Diagnostics = append(out.Diagnostics, FactDiagnostic{Code: diag.Code, Detail: diag.Detail})
		}
	}
	return out
}

func versionPeopleAPI(f store.DocumentPeopleFacts, safe bool) VersionPeople {
	out := VersionPeople{ContentVersionID: f.Version.ID, State: f.State, Edges: []VersionPersonEdge{}, Assertions: []VersionPersonAssertion{}, Unresolved: []VersionUnresolvedName{}}
	for _, e := range f.Edges {
		p, ok := f.Persons[e.PersonID]
		if !ok || safe && !p.Visible {
			continue
		}
		name := p.Person.DisplayName
		if safe && p.Person.State == "provisional" && p.Person.Origin == "derived" &&
			(e.Basis != "operator_assigned" || (e.EvidenceKind != "operator_assertion" && e.EvidenceKind != "custodian_assignment")) {
			name = e.RawLabel
		}
		edge := VersionPersonEdge{PersonID: e.PersonID, DisplayName: name, Role: e.Role, Basis: e.Basis, Confidence: e.Confidence, EvidenceKind: e.EvidenceKind, EvidenceID: e.EvidenceID, RawLabel: e.RawLabel, ClaimCount: e.ClaimCount, External: []FactExternal{}}
		for _, x := range p.External {
			edge.External = append(edge.External, FactExternal{System: x.System, ArchiveID: x.ArchiveID, UID: x.UID})
		}
		edge.LinkedContact = len(edge.External) > 0
		out.Edges = append(out.Edges, edge)
	}
	for _, a := range f.Assertions {
		p, ok := f.Persons[a.PersonID]
		if !ok || safe && !p.Visible {
			continue
		}
		out.Assertions = append(out.Assertions, VersionPersonAssertion{AssertionID: a.AssertionID, PersonID: a.PersonID, DisplayName: p.Person.DisplayName, Role: a.Role, Action: a.Action, Note: a.Note, RecordedAt: a.RecordedAt, Revision: a.Revision})
	}
	for _, c := range f.Candidates {
		for _, o := range c.Occurrences {
			out.Unresolved = append(out.Unresolved, VersionUnresolvedName{RawLabel: o.Label, Role: o.Role, CandidateID: c.CandidateID, EvidenceKind: o.EvidenceKind})
		}
	}
	return out
}

func rejectFactsUnknownQuery(ctx huma.Context, next func(huma.Context)) {
	url := ctx.URL()
	for key := range url.Query() {
		if key != "disclosure" {
			_, w := humago.Unwrap(ctx)
			writeError(w, NewError(http.StatusBadRequest, "unknown_parameter", "Unknown query parameter"))
			return
		}
	}
	next(ctx)
}
