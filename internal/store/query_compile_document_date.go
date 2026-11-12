package store

import (
	"fmt"
	"slices"
	"strings"

	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/internal/query"
	"go.kenn.io/kit/civiltime"
)

func datePrecisionGroup(values []string) [][]string {
	if len(values) == 0 {
		return nil
	}
	return [][]string{values}
}

type dateScope struct {
	ranges          [][2]string
	kinds           []string
	precisionGroups [][]string
}

func (scope *dateScope) add(field, value string) error {
	switch field {
	case "date":
		from, to, err := query.ParseDocumentDate(value)
		if err != nil {
			return err
		}
		scope.ranges = append(scope.ranges, [2]string{from, to})
	case "date_kind":
		if !document.ValidDateKind(document.DateKind(value)) {
			return fmt.Errorf("unknown date kind %q", value)
		}
		scope.kinds = append(scope.kinds, value)
	case "date_precision":
		if !slices.Contains([]string{"exact", "day", "coarse"}, value) {
			return fmt.Errorf("unknown date precision %q", value)
		}
		scope.precisionGroups = append(scope.precisionGroups, []string{value})
	}
	return nil
}

func (c queryCompiler) compileDateScope(expression *query.ResolvedExpression, filter *query.DocumentDateFilter) (compiledQueryFragment, error) {
	var scope dateScope
	if filter != nil {
		scope.ranges = append(scope.ranges, [2]string{filter.From, filter.To})
		scope.kinds = append(scope.kinds, filter.Kinds...)
		if len(filter.Precisions) > 0 {
			scope.precisionGroups = append(scope.precisionGroups, filter.Precisions)
		}
	}
	var parts []compiledQueryFragment
	var collect func(*query.ResolvedExpression) error
	collect = func(node *query.ResolvedExpression) error {
		if node.Syntax.Kind == query.ExpressionAnd {
			for _, child := range node.Children {
				if err := collect(child); err != nil {
					return err
				}
			}
			return nil
		}
		if node.Syntax.Kind == query.ExpressionField && slices.Contains([]string{"date", "date_kind", "date_precision"}, node.Syntax.Field) {
			if len(node.Children) != 1 || (node.Children[0].Syntax.Kind != query.ExpressionTerm && node.Children[0].Syntax.Kind != query.ExpressionPhrase) || node.Children[0].Syntax.Prefix {
				return compileExpressionError(node.Syntax.Start, node.Syntax.End, "date operand must be a single value")
			}
			if err := scope.add(node.Syntax.Field, node.Children[0].Syntax.Value); err != nil {
				return compileExpressionError(node.Syntax.Start, node.Syntax.End, err.Error())
			}
			return nil
		}
		part, err := c.compileResolvedExpression(node, "")
		if err != nil {
			return err
		}
		parts = append(parts, part)
		return nil
	}
	if err := collect(expression); err != nil {
		return compiledQueryFragment{}, err
	}
	if len(scope.ranges) > 0 || len(scope.kinds) > 0 || len(scope.precisionGroups) > 0 {
		part, err := c.compileDatePredicate(scope)
		if err != nil {
			return compiledQueryFragment{}, err
		}
		parts = append(parts, part)
	}
	return joinCompiledFragments(parts, ` AND `), nil
}

func (c queryCompiler) compileDatePredicate(scope dateScope) (compiledQueryFragment, error) {
	zone := c.dateZone
	if zone == "" {
		zone = "UTC"
	}
	loc, err := civiltime.LoadZone(zone)
	if err != nil {
		return compiledQueryFragment{}, fmt.Errorf("invalid_timezone: %w", err)
	}
	predicates := []string{"k.generation_id=h.generation_id", "h.content_version_id=cv.version_id", "k.sensitive=0"}
	var args []any
	if len(scope.kinds) == 0 && len(scope.ranges) > 0 {
		predicates = append(predicates, "k.primary_safe=1")
	}
	if len(scope.kinds) > 0 {
		var tests []string
		for _, kind := range scope.kinds {
			tests = append(tests, "k.date_kind=?")
			args = append(args, kind)
		}
		predicates = append(predicates, "("+strings.Join(tests, " OR ")+")")
	}
	for _, group := range scope.precisionGroups {
		var alternatives []string
		for _, precision := range group {
			switch precision {
			case "exact":
				alternatives = append(alternatives, "k.precision IN ('hour','minute','second','fraction')")
			case "day":
				alternatives = append(alternatives, "k.precision='date'")
			case "coarse":
				alternatives = append(alternatives, "k.precision IN ('year','month')")
			}
		}
		predicates = append(predicates, "("+strings.Join(alternatives, " OR ")+")")
	}

	for _, r := range scope.ranges {
		from, _ := civiltime.ParseDate(r[0])
		to, _ := civiltime.ParseDate(r[1])
		start, end, err := civiltime.RangeBounds(from, to, loc)
		if err != nil {
			return compiledQueryFragment{}, fmt.Errorf("date_not_in_zone: %w", err)
		}
		format := "2006-01-02T15:04:05.000000000Z"
		predicates = append(predicates, `((k.utc_key IS NOT NULL AND k.utc_key>=? AND k.utc_key<?) OR
			(k.utc_key IS NULL AND k.precision NOT IN ('year','month') AND k.civil_date<=? AND k.civil_end_date>=?) OR
			(k.precision='month' AND k.civil_month>=? AND k.civil_month<=?) OR
			(k.precision='year' AND k.civil_year>=? AND k.civil_year<=?))`)
		args = append(args, start.UTC().Format(format), end.UTC().Format(format), r[1], r[0], r[0][:7], r[1][:7], r[0][:4], r[1][:4])
	}
	return compiledQueryFragment{sql: `EXISTS (SELECT 1 FROM document_date_keys k JOIN document_event_heads h ON h.generation_id=k.generation_id WHERE ` + strings.Join(predicates, " AND ") + ")", args: args}, nil
}
