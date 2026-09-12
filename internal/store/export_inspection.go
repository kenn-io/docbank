package store

import (
	"context"
	"database/sql"
	"slices"
	"strings"

	"go.kenn.io/docbank/document/bundle"
	"go.kenn.io/docbank/internal/canonical"
)

// ExportEmailPDFRecipes is advisory. Plan sealing independently selects and pins
// exact receipts; a discovery response never authorizes substituting a recipe.
func (s *Store) ExportEmailPDFRecipes(ctx context.Context, owner, id string) (bundle.EmailPDFRecipes, error) {
	var out bundle.EmailPDFRecipes
	if owner == "" || validateUUIDv4(id) != nil {
		return out, bundle.ErrConflict
	}
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return out, err
	}
	defer func() { _ = tx.Rollback() }()
	source, err := loadExportSource(ctx, tx, owner, id)
	if err != nil {
		return out, err
	}
	if exportExpired(source.ExpiresAt) {
		return out, bundle.ErrExpired
	}
	if source.State != exportSealedState {
		return out, bundle.ErrConflict
	}
	out = bundle.EmailPDFRecipes{SourceID: id, MemberHash: source.MemberHash, Total: source.Total, Recipes: []bundle.EmailPDFRecipeChoice{}}
	choices := map[string]bundle.EmailPDFRecipeChoice{}
	scanned := 0
	err = walkExportMembers(ctx, tx, id, func(m bundle.Member) error {
		counts := map[string]int{}
		after := ""
		for {
			profiles, e := exportEmailPDFProfiles(ctx, tx, m.VersionID, after)
			if e != nil {
				return e
			}
			for _, profile := range profiles {
				scanned++
				if scanned > bundle.MaxRoles {
					return bundle.ErrLimit
				}
				r, e := emailPDFReceipt(ctx, tx, m.VersionID, profile)
				if e != nil {
					return e
				}
				raw, e := canonical.Marshal(r.Binding.Recipe)
				if e != nil {
					return e
				}
				sha := pageChecksum(raw)
				if _, ok := choices[sha]; !ok {
					if len(choices) >= bundle.InspectionPageSize {
						return bundle.ErrLimit
					}
					choices[sha] = bundle.EmailPDFRecipeChoice{RecipeSHA256: sha, Paper: r.Binding.Recipe.Paper, RendererVersion: r.Binding.Recipe.RendererVersion}
				}
				counts[sha]++
			}
			if len(profiles) < 100 {
				break
			}
			after = profiles[len(profiles)-1]
		}
		for sha, count := range counts {
			choice := choices[sha]
			if count == 1 {
				choice.Messages++
			} else {
				choice.Ambiguous++
			}
			choices[sha] = choice
		}
		return nil
	})
	if err != nil {
		return bundle.EmailPDFRecipes{}, err
	}
	for _, choice := range choices {
		out.Recipes = append(out.Recipes, choice)
	}
	slices.SortFunc(out.Recipes, func(a, b bundle.EmailPDFRecipeChoice) int { return strings.Compare(a.RecipeSHA256, b.RecipeSHA256) })
	if err = tx.Commit(); err != nil {
		return bundle.EmailPDFRecipes{}, err
	}
	return out, nil
}

// ExportOutputProblems retains at most 50 scalar problems, including incomplete
// inventories. The cursor addresses the immutable plan, never current heads.
func (s *Store) ExportOutputProblems(ctx context.Context, owner, id string, after int) (bundle.OutputProblems, error) {
	var out bundle.OutputProblems
	if owner == "" || validateUUIDv4(id) != nil || after < 0 || after > bundle.MaxOutputProblems {
		return out, bundle.ErrConflict
	}
	p, err := s.ExportPlan(ctx, owner, id)
	if err != nil {
		return out, err
	}
	out = bundle.OutputProblems{PlanID: p.ID, Fingerprint: p.Fingerprint, After: after, Items: []bundle.OutputProblem{}}
	validator := bundle.RowValidator{Plan: p}
	appendProblem := func(d bundle.Document, role, reason string) {
		out.Total++
		if out.Total <= after || len(out.Items) >= bundle.InspectionPageSize {
			return
		}
		problem := bundle.OutputProblem{NodeID: d.NodeID, VersionID: d.VersionID, Role: role, Reason: reason}
		if d.Attachment != nil {
			problem.PartPath = d.Attachment.PartPath
		}
		out.Items = append(out.Items, problem)
	}
	err = s.WalkExportDocuments(ctx, id, func(d bundle.Document) error {
		if e := validator.Add(d); e != nil {
			return e
		}
		if d.Inventory != nil && d.Inventory.State != exportInventoryComplete {
			for _, policy := range p.Roles {
				if policy.Role == "attachment_original" || policy.Role == "attachment_pdf" {
					appendProblem(d, policy.Role, "Attachment inventory is "+d.Inventory.State+"; known occurrences remain declared.")
				}
			}
		}
		for _, role := range d.Roles {
			if role.Status != exportRoleUnavailable {
				continue
			}
			reason := role.Reason
			if reason == "" {
				reason = "No eligible retained output for this exact version and selected policy."
			}
			appendProblem(d, role.Role, reason)
		}
		return nil
	})
	if err != nil {
		return bundle.OutputProblems{}, err
	}
	if err = validator.Finish(); err != nil {
		return bundle.OutputProblems{}, err
	}
	if after > out.Total {
		return bundle.OutputProblems{}, bundle.ErrConflict
	}
	if after+len(out.Items) < out.Total {
		out.Next = after + len(out.Items)
	}
	raw, err := canonical.Marshal(out)
	if err != nil {
		return bundle.OutputProblems{}, err
	}
	if len(raw) > bundle.MaxMemberBytes {
		return bundle.OutputProblems{}, bundle.ErrLimit
	}
	return out, nil
}
