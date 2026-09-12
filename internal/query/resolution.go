package query

import (
	"context"
	"errors"
	"fmt"
	"slices"
)

const (
	maxResolvedExpressionNodes = 4096
	maxSavedReferenceDepth     = 16
	maxResolvedDependencies    = 256
	maxResolvedCanonicalBytes  = 256 << 10
)

// ReferenceKind identifies an externally resolved query operand.
type ReferenceKind string

const (
	ReferenceTag        ReferenceKind = "tag"
	ReferenceCollection ReferenceKind = "collection"
	ReferenceSaved      ReferenceKind = "saved"
)

// ErrUnknownReference identifies a recognized reference value which does not
// exist. ResolveQuery turns it into a positioned ExpressionError.
var ErrUnknownReference = errors.New("unknown query reference")

// Dependency identifies the stable revision used while resolving a query.
type Dependency struct {
	Kind     ReferenceKind
	ID       string
	Revision int64
}

// Reference is the validated result of one resolver lookup. Query is required
// only for saved-query references.
type Reference struct {
	Dependency Dependency
	Query      *Query
}

// Resolver resolves either an exact stable ID or a decoded display name.
type Resolver interface {
	Resolve(ctx context.Context, kind ReferenceKind, key string, byID bool) (Reference, error)
}

// ResolvedExpression mirrors a parsed expression and decorates reference
// leaves with stable dependencies and saved-query expansions.
type ResolvedExpression struct {
	Syntax     *Expression
	Children   []*ResolvedExpression
	Dependency *Dependency
	Saved      *ResolvedQuery
}

// ResolvedQuery retains one normalized query and its scope-local expansion.
type ResolvedQuery struct {
	Query        Query
	Expression   *ResolvedExpression
	Dependencies []Dependency
}

type dependencyKey struct {
	kind ReferenceKind
	id   string
}

type resolutionState struct {
	resolver        Resolver
	activeSaved     map[string]struct{}
	revisions       map[dependencyKey]int64
	expressionNodes int
	canonicalBytes  int
}

// ResolveQuery normalizes a defensive copy and expands all references without
// changing the query's entered text or lifting nested saved-query filters.
func ResolveQuery(ctx context.Context, value Query, resolver Resolver) (ResolvedQuery, error) {
	if err := ctx.Err(); err != nil {
		return ResolvedQuery{}, err
	}
	normalized, canonicalBytes, err := normalizedQueryCopy(value)
	if err != nil {
		return ResolvedQuery{}, err
	}
	state := resolutionState{
		resolver:       resolver,
		activeSaved:    make(map[string]struct{}),
		revisions:      make(map[dependencyKey]int64),
		canonicalBytes: canonicalBytes,
	}
	return state.resolveQuery(ctx, normalized, 0)
}

func normalizedQueryCopy(value Query) (Query, int, error) {
	canonical, err := Canonical(value)
	if err != nil {
		return Query{}, 0, err
	}
	normalized, err := Parse(canonical)
	if err != nil {
		return Query{}, 0, err
	}
	return normalized, len(canonical), nil
}

func (state *resolutionState) resolveQuery(ctx context.Context, value Query, savedDepth int) (ResolvedQuery, error) {
	if err := ctx.Err(); err != nil {
		return ResolvedQuery{}, err
	}
	if value.Mode != "lexical" {
		return ResolvedQuery{}, expressionError(0, len(value.Text), "reference expansion requires lexical query mode")
	}
	syntax, err := ParseExpression(value.Text, value.Syntax)
	if err != nil {
		return ResolvedQuery{}, err
	}
	expression, expressionDependencies, err := state.resolveExpression(ctx, syntax, "", savedDepth)
	if err != nil {
		return ResolvedQuery{}, err
	}
	facetDependencies, err := state.resolveFacets(ctx, value)
	if err != nil {
		return ResolvedQuery{}, err
	}
	return ResolvedQuery{
		Query:        value,
		Expression:   expression,
		Dependencies: sortedDependencies(expressionDependencies, facetDependencies),
	}, nil
}

func (state *resolutionState) resolveExpression(
	ctx context.Context,
	syntax *Expression,
	field string,
	savedDepth int,
) (*ResolvedExpression, []Dependency, error) {
	if err := ctx.Err(); err != nil {
		return nil, nil, err
	}
	state.expressionNodes++
	if state.expressionNodes > maxResolvedExpressionNodes {
		return nil, nil, expressionError(syntax.Start, syntax.End, "expanded expression exceeds 4096 AST nodes")
	}

	resolved := &ResolvedExpression{Syntax: syntax}
	if syntax.Kind == ExpressionField {
		if field != "" {
			return nil, nil, expressionError(syntax.Start, syntax.End, "nested field overrides are not supported")
		}
		children, dependencies, err := state.resolveChildren(ctx, syntax, syntax.Field, savedDepth)
		if err != nil {
			return nil, nil, err
		}
		resolved.Children = children
		return resolved, dependencies, nil
	}

	if kind, isReference := referenceKindForField(field); isReference {
		switch syntax.Kind {
		case ExpressionTerm, ExpressionPhrase:
			if syntax.Prefix {
				return nil, nil, expressionError(syntax.Start, syntax.End, "reference operands cannot use prefix matching")
			}
			dependency, saved, dependencies, err := state.resolveLeaf(ctx, kind, syntax, savedDepth)
			if err != nil {
				return nil, nil, err
			}
			resolved.Dependency = dependency
			resolved.Saved = saved
			return resolved, dependencies, nil
		case ExpressionAnd, ExpressionOr, ExpressionNot:
			children, dependencies, err := state.resolveChildren(ctx, syntax, field, savedDepth)
			if err != nil {
				return nil, nil, err
			}
			resolved.Children = children
			return resolved, dependencies, nil
		case ExpressionNear:
			return nil, nil, expressionError(syntax.Start, syntax.End, "reference operands cannot use NEAR")
		default:
			return nil, nil, expressionError(syntax.Start, syntax.End, "reference field requires exact term or phrase operands")
		}
	}

	children, dependencies, err := state.resolveChildren(ctx, syntax, field, savedDepth)
	if err != nil {
		return nil, nil, err
	}
	resolved.Children = children
	return resolved, dependencies, nil
}

func (state *resolutionState) resolveChildren(
	ctx context.Context,
	syntax *Expression,
	field string,
	savedDepth int,
) ([]*ResolvedExpression, []Dependency, error) {
	if len(syntax.Children) == 0 {
		return nil, nil, nil
	}
	children := make([]*ResolvedExpression, 0, len(syntax.Children))
	var dependencies []Dependency
	for _, childSyntax := range syntax.Children {
		child, childDependencies, err := state.resolveExpression(ctx, childSyntax, field, savedDepth)
		if err != nil {
			return nil, nil, err
		}
		children = append(children, child)
		dependencies = append(dependencies, childDependencies...)
	}
	return children, dependencies, nil
}

func (state *resolutionState) resolveLeaf(
	ctx context.Context,
	kind ReferenceKind,
	syntax *Expression,
	savedDepth int,
) (*Dependency, *ResolvedQuery, []Dependency, error) {
	if kind == ReferenceSaved && savedDepth >= maxSavedReferenceDepth {
		return nil, nil, nil, expressionError(syntax.Start, syntax.End, "saved reference nesting exceeds 16")
	}
	ref, err := state.lookup(ctx, kind, syntax.Value, validUUIDv4(syntax.Value), syntax.Start, syntax.End)
	if err != nil {
		return nil, nil, nil, err
	}
	dependency := ref.Dependency
	dependencies := []Dependency{dependency}
	if kind != ReferenceSaved {
		return &dependency, nil, dependencies, nil
	}

	if _, active := state.activeSaved[dependency.ID]; active {
		return nil, nil, nil, expressionError(syntax.Start, syntax.End, "saved reference cycle detected")
	}
	canonical, err := Canonical(*ref.Query)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("normalize saved query %s: %w", dependency.ID, err)
	}
	if len(canonical) > maxResolvedCanonicalBytes-state.canonicalBytes {
		return nil, nil, nil, expressionError(syntax.Start, syntax.End, "expanded queries exceed 256 KiB")
	}
	state.canonicalBytes += len(canonical)
	nested, err := Parse(canonical)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("normalize saved query %s: %w", dependency.ID, err)
	}
	state.activeSaved[dependency.ID] = struct{}{}
	nestedResolved, err := state.resolveQuery(ctx, nested, savedDepth+1)
	delete(state.activeSaved, dependency.ID)
	if err != nil {
		if nestedExpressionErr, ok := errors.AsType[*ExpressionError](err); ok {
			return nil, nil, nil, expressionError(syntax.Start, syntax.End, nestedExpressionErr.Message)
		}
		return nil, nil, nil, err
	}
	dependencies = append(dependencies, nestedResolved.Dependencies...)
	return &dependency, &nestedResolved, dependencies, nil
}

func (state *resolutionState) resolveFacets(ctx context.Context, value Query) ([]Dependency, error) {
	type facet struct {
		kind   ReferenceKind
		values []string
	}
	facets := []facet{
		{kind: ReferenceCollection, values: value.Filters.CollectionIDs},
		{kind: ReferenceCollection, values: value.Filters.ExcludeCollectionIDs},
		{kind: ReferenceTag, values: value.Filters.TagIDs},
		{kind: ReferenceTag, values: value.Filters.ExcludeTagIDs},
	}
	var dependencies []Dependency
	for _, facet := range facets {
		for _, id := range facet.values {
			ref, err := state.lookup(ctx, facet.kind, id, true, 0, len(value.Text))
			if err != nil {
				return nil, err
			}
			dependencies = append(dependencies, ref.Dependency)
		}
	}
	return dependencies, nil
}

func (state *resolutionState) lookup(
	ctx context.Context,
	kind ReferenceKind,
	key string,
	byID bool,
	start int,
	end int,
) (Reference, error) {
	if err := ctx.Err(); err != nil {
		return Reference{}, err
	}
	if state.resolver == nil {
		return Reference{}, errors.New("query resolver is required for references")
	}
	ref, err := state.resolver.Resolve(ctx, kind, key, byID)
	if err != nil {
		if errors.Is(err, ErrUnknownReference) {
			return Reference{}, expressionError(start, end, fmt.Sprintf("unknown %s reference", kind))
		}
		return Reference{}, fmt.Errorf("resolve %s reference: %w", kind, err)
	}
	if err := validateReference(ref, kind, key, byID); err != nil {
		return Reference{}, err
	}
	if err := state.recordDependency(ref.Dependency); err != nil {
		if limitErr, ok := errors.AsType[*dependencyLimitError](err); ok {
			return Reference{}, expressionError(start, end, limitErr.Error())
		}
		return Reference{}, err
	}
	return ref, nil
}

func validateReference(ref Reference, kind ReferenceKind, key string, byID bool) error {
	dependency := ref.Dependency
	if dependency.Kind != kind {
		return fmt.Errorf("resolver returned %q kind for %q reference", dependency.Kind, kind)
	}
	if !validUUIDv4(dependency.ID) {
		return fmt.Errorf("resolver returned invalid stable ID for %s reference", kind)
	}
	if byID && dependency.ID != key {
		return fmt.Errorf("resolver returned stable ID %q for exact ID %q", dependency.ID, key)
	}
	if dependency.Revision <= 0 {
		return fmt.Errorf("resolver returned nonpositive revision for %s reference", kind)
	}
	if kind == ReferenceSaved && ref.Query == nil {
		return errors.New("resolver returned saved reference without a query")
	}
	return nil
}

type dependencyLimitError struct{}

func (*dependencyLimitError) Error() string {
	return "expanded query exceeds 256 distinct dependencies"
}

func (state *resolutionState) recordDependency(dependency Dependency) error {
	key := dependencyKey{kind: dependency.Kind, id: dependency.ID}
	if revision, exists := state.revisions[key]; exists {
		if revision != dependency.Revision {
			return fmt.Errorf(
				"resolver returned conflicting revisions %d and %d for %s %s",
				revision, dependency.Revision, dependency.Kind, dependency.ID,
			)
		}
		return nil
	}
	if len(state.revisions) >= maxResolvedDependencies {
		return &dependencyLimitError{}
	}
	state.revisions[key] = dependency.Revision
	return nil
}

func referenceKindForField(field string) (ReferenceKind, bool) {
	switch field {
	case string(ReferenceTag):
		return ReferenceTag, true
	case string(ReferenceCollection):
		return ReferenceCollection, true
	case string(ReferenceSaved):
		return ReferenceSaved, true
	default:
		return "", false
	}
}

func sortedDependencies(groups ...[]Dependency) []Dependency {
	byKey := make(map[dependencyKey]Dependency)
	for _, dependencies := range groups {
		for _, dependency := range dependencies {
			byKey[dependencyKey{kind: dependency.Kind, id: dependency.ID}] = dependency
		}
	}
	result := make([]Dependency, 0, len(byKey))
	for _, dependency := range byKey {
		result = append(result, dependency)
	}
	slices.SortFunc(result, func(left, right Dependency) int {
		if left.Kind < right.Kind {
			return -1
		}
		if left.Kind > right.Kind {
			return 1
		}
		if left.ID < right.ID {
			return -1
		}
		if left.ID > right.ID {
			return 1
		}
		return 0
	})
	if len(result) == 0 {
		return nil
	}
	return result
}
