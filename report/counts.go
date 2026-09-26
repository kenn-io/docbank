package report

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	"go.kenn.io/docbank/document"
)

type familyKey struct {
	id        string
	singleton Identity
}

type familyFlags struct {
	hit        bool
	uniqueHit  bool
	competitor bool
}

// Calculate derives date-eligible hits and all five distinct-document counts
// from one frozen frame. It never queries live vault state or changes its input.
func Calculate(ctx context.Context, budget Budget, frame Frame) (_ Result, err error) {
	if err := ctx.Err(); err != nil {
		return Result{}, err
	}
	if budget == nil {
		return Result{}, errors.New("missing report budget")
	}
	request, err := NormalizeRequest(frame.Request)
	if err != nil {
		return Result{}, err
	}
	if len(frame.Members) > 50000 || len(frame.Relations) > 100000 {
		return Result{}, fmt.Errorf("%w: report population exceeds limit", ErrReportLimit)
	}
	terms := len(request.Terms)
	memberCount := len(frame.Members)
	retainedBytes := int64(memberCount)*(512+int64(terms)*2) + int64(terms)*1024
	for _, term := range request.Terms {
		retainedBytes += int64(len(term.Expression))
	}
	releaseRetained, err := budget.Reserve(ctx, retainedBytes)
	if err != nil {
		return Result{}, err
	}
	defer func() {
		if err != nil {
			releaseRetained()
		}
	}()
	scratch := budget.Child()
	defer func() { _ = scratch.Close() }()
	releaseScratch, err := scratch.Reserve(ctx, int64(memberCount)*256+int64(terms)*128)
	if err != nil {
		return Result{}, err
	}
	defer releaseScratch()

	result := Result{Frame: frame, Counts: make([]Counts, terms)}
	result.Frame.Coverage = Coverage{Warnings: slices.Clone(frame.Coverage.Warnings)}
	result.Frame.RowCoverage = make([]Coverage, terms)
	result.Frame.Request = request
	result.Frame.Members = make([]Member, memberCount)
	copy(result.Frame.Members, frame.Members)
	eligibleBits := make([]bool, memberCount*terms)
	hitBits := make([]bool, memberCount*terms)
	dateStarts := make([]time.Time, terms)
	dateEnds := make([]time.Time, terms)
	for row, term := range request.Terms {
		dateStarts[row], _ = parseISODate(term.Dates.Start)
		dateEnds[row], _ = parseISODate(term.Dates.End)
	}

	selectedCollections := make(map[string]bool, len(request.CollectionIDs))
	for _, id := range request.CollectionIDs {
		selectedCollections[id] = true
	}
	selectedDocuments := make(map[Identity]bool, len(request.SelectedDocuments))
	for _, identity := range request.SelectedDocuments {
		selectedDocuments[identity] = true
	}
	if len(selectedDocuments) > 0 && memberCount != len(selectedDocuments) {
		return Result{}, errors.New("sealed report member count differs")
	}
	identities := make(map[Identity]int, memberCount)
	families := make(map[familyKey]int, memberCount)
	familyForMember := make([]int, memberCount)
	for index, member := range frame.Members {
		if index%1024 == 0 {
			if err := ctx.Err(); err != nil {
				return Result{}, err
			}
		}
		if !document.ValidDocumentKind(document.DocumentKind(member.Kind)) || member.Identity.NodeID <= 0 || member.Identity.VersionID == "" ||
			!validSHA256(member.Identity.SHA256) {
			return Result{}, fmt.Errorf("member %d has invalid current-document identity", index)
		}
		if _, exists := identities[member.Identity]; exists {
			return Result{}, errors.New("duplicate report document identity")
		}
		identities[member.Identity] = index
		if len(member.RawMatches) != terms {
			return Result{}, fmt.Errorf("member %d has %d match bits, need %d", index, len(member.RawMatches), terms)
		}
		if len(selectedDocuments) > 0 && !selectedDocuments[member.Identity] {
			return Result{}, fmt.Errorf("member %d is outside sealed selection", index)
		}
		if len(selectedCollections) > 0 {
			matched := false
			for _, witness := range member.CollectionWitnesses {
				if selectedCollections[witness.CollectionID] && witness.MembershipID != "" && validSHA256(witness.MembershipSHA256) {
					matched = true
				}
			}
			if !matched {
				return Result{}, fmt.Errorf("member %d lacks selected collection witness", index)
			}
		}
		key := familyKey{id: member.FamilyID}
		if key.id == "" {
			key.singleton = member.Identity
		}
		family, found := families[key]
		if !found {
			family = len(families)
			families[key] = family
		}
		familyForMember[index] = family
		result.Frame.Members[index].Eligible = eligibleBits[index*terms : (index+1)*terms]
		result.Frame.Members[index].Hits = hitBits[index*terms : (index+1)*terms]
		if member.Selection.Date == "" {
			if request.CoverageMode == "strict" {
				return Result{}, fmt.Errorf("member %d: %w", index, ErrUnusableDate)
			}
			continue
		}
		date, dateErr := parseISODate(member.Selection.Date)
		if dateErr != nil {
			return Result{}, fmt.Errorf("member %d has invalid selected date: %w", index, dateErr)
		}
		included := false
		for row := range request.Terms {
			eligible := !date.Before(dateStarts[row]) && !date.After(dateEnds[row])
			result.Frame.Members[index].Eligible[row] = eligible
			result.Frame.Members[index].Hits[row] = eligible && member.RawMatches[row]
			if eligible {
				included = true
				chargeCoverageMember(&result.Frame.RowCoverage[row], member)
			}
		}
		if included {
			chargeCoverageMember(&result.Frame.Coverage, member)
		}
	}
	for _, relation := range frame.Relations {
		parent, hasParent := identities[relation.Parent]
		child, hasChild := identities[relation.Child]
		if hasParent && hasChild && familyForMember[parent] != familyForMember[child] {
			return Result{}, errors.New("relation endpoints have inconsistent family identity")
		}
	}

	hitCount := make([]int, memberCount)
	for index, member := range result.Frame.Members {
		for _, hit := range member.Hits {
			if hit {
				hitCount[index]++
			}
		}
	}
	for row := range request.Terms {
		if err := ctx.Err(); err != nil {
			return Result{}, err
		}
		flags := make([]familyFlags, len(families))
		for index, member := range result.Frame.Members {
			if index%1024 == 0 {
				if err := ctx.Err(); err != nil {
					return Result{}, err
				}
			}
			if !member.Eligible[row] {
				continue
			}
			family := &flags[familyForMember[index]]
			otherHit := hitCount[index] > 0
			if member.Hits[row] {
				result.Counts[row].Hits++
				family.hit = true
				otherHit = hitCount[index] > 1
				if !otherHit {
					result.Counts[row].UniqueHits++
					family.uniqueHit = true
				}
			}
			if otherHit {
				family.competitor = true
			}
		}
		for index, member := range result.Frame.Members {
			if !member.Eligible[row] {
				continue
			}
			family := flags[familyForMember[index]]
			if family.hit {
				result.Counts[row].HitsPlusFamily++
				if !family.competitor {
					result.Counts[row].UniqueFamilies++
				}
			}
			if family.uniqueHit {
				result.Counts[row].UniqueHitsPlusFamily++
			}
		}
	}
	return result, nil
}

func chargeCoverageMember(coverage *Coverage, member Member) {
	coverage.Scoped++
	if member.Coverage.SearchState == StateComplete {
		coverage.Searchable++
	} else {
		coverage.MissingText++
	}
	if member.Coverage.FamilyState != StateComplete {
		coverage.IncompleteFamilies++
	}
	if member.Selection.Reason == "vault_addition" || member.Selection.Reason == "recorded_fallback" {
		coverage.FallbackDates++
	}
}
