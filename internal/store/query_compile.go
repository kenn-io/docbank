package store

import (
	"context"
	"errors"
	"strconv"
	"strings"

	"go.kenn.io/docbank/internal/query"
)

const (
	maxCompiledQuerySQL  = 256 << 10
	maxCompiledQueryArgs = 4096
	compiledNameField    = "name"
)

type compiledGenerationArgument struct{}

type compiledQueryFragment struct {
	sql  string
	args []any
}

// CompiledQuery is one resolved QueryV1 value compiled into an internal,
// parameterized document-membership predicate.
type CompiledQuery struct {
	Query        query.Query
	Dependencies []query.Dependency

	predicate compiledQueryFragment
}

// compileQuery resolves references and compiles one bounded query without
// selecting a lexical generation or accessing SQLite.
func compileQuery(
	ctx context.Context, value query.Query, resolver query.Resolver,
) (CompiledQuery, error) {
	resolved, err := query.ResolveQuery(ctx, value, resolver)
	if err != nil {
		return CompiledQuery{}, err
	}
	predicate, err := compileResolvedQuery(resolved)
	if err != nil {
		return CompiledQuery{}, err
	}
	compiled := CompiledQuery{
		Query: resolved.Query, Dependencies: resolved.Dependencies,
		predicate: predicate,
	}
	if _, _, err := compiled.Bind(""); err != nil {
		return CompiledQuery{}, err
	}
	return compiled, nil
}

func compileResolvedQuery(resolved query.ResolvedQuery) (compiledQueryFragment, error) {
	if resolved.Query.Sort.Field == "relevance" {
		return compiledQueryFragment{},
			compileExpressionError(0, len(resolved.Query.Text), "relevance sort is not supported by bound queries")
	}
	if len(resolved.Query.Filters.TextCoverage) != 0 {
		return compiledQueryFragment{},
			compileExpressionError(0, len(resolved.Query.Text), "text_coverage is not supported by bound queries")
	}
	if resolved.Query.Filters.HasDuplicates {
		return compiledQueryFragment{},
			compileExpressionError(0, len(resolved.Query.Text), "has_duplicates is not supported by bound queries")
	}
	if resolved.Query.Filters.CollapseDuplicates {
		return compiledQueryFragment{},
			compileExpressionError(0, len(resolved.Query.Text), "duplicate collapsing is not supported by bound queries")
	}

	var expression compiledQueryFragment
	var err error
	if resolved.Query.Syntax == "simple" {
		fts := ftsQuery(resolved.Query.Text)
		if fts == "" {
			expression = trueCompiledFragment()
		} else {
			expression = compileLexicalPredicate(fts, true)
		}
	} else {
		expression, err = compileResolvedExpression(resolved.Expression, "")
		if err != nil {
			return compiledQueryFragment{}, err
		}
	}
	filters, err := compileQueryFilters(resolved.Query.Filters, 0, len(resolved.Query.Text))
	if err != nil {
		return compiledQueryFragment{}, err
	}
	return joinCompiledFragments([]compiledQueryFragment{expression, filters}, ` AND `), nil
}

// Bind renders the predicate for one caller-selected lexical generation.
func (compiled CompiledQuery) Bind(generationID string) (string, []any, error) {
	predicate := joinCompiledFragments([]compiledQueryFragment{{
		sql: `n.kind='file' AND n.trashed_at IS NULL AND cv.node_id=n.id AND cv.version_id=n.current_version_id`,
	}, compiled.predicate}, ` AND `)
	args := make([]any, len(predicate.args))
	for index, arg := range predicate.args {
		if _, marker := arg.(compiledGenerationArgument); marker {
			args[index] = generationID
		} else {
			args[index] = arg
		}
	}
	if len(predicate.sql) > maxCompiledQuerySQL || len(args) > maxCompiledQueryArgs {
		return "", nil, compileExpressionError(
			0, len(compiled.Query.Text), "compiled query exceeds 256 KiB or 4096 arguments",
		)
	}
	return predicate.sql, args, nil
}

func compileResolvedExpression(expression *query.ResolvedExpression, field string) (compiledQueryFragment, error) {
	if expression == nil || expression.Syntax == nil {
		return compiledQueryFragment{}, errors.New("resolved query contains an empty expression")
	}
	syntax := expression.Syntax
	switch syntax.Kind {
	case query.ExpressionAll:
		if field != "" {
			return compiledQueryFragment{}, compileExpressionError(syntax.Start, syntax.End, "field operand cannot be empty")
		}
		return trueCompiledFragment(), nil
	case query.ExpressionField:
		if len(expression.Children) != 1 {
			return compiledQueryFragment{}, errors.New("resolved field expression has invalid children")
		}
		return compileResolvedExpression(expression.Children[0], syntax.Field)
	case query.ExpressionAnd, query.ExpressionOr:
		parts, err := compileAssociativeChildren(expression, field, syntax.Kind)
		if err != nil {
			return compiledQueryFragment{}, err
		}
		operator := ` AND `
		if syntax.Kind == query.ExpressionOr {
			operator = ` OR `
		}
		return joinCompiledFragments(parts, operator), nil
	case query.ExpressionNot:
		if len(expression.Children) != 1 {
			return compiledQueryFragment{}, errors.New("resolved NOT expression has invalid children")
		}
		child, err := compileResolvedExpression(expression.Children[0], field)
		if err != nil {
			return compiledQueryFragment{}, err
		}
		return compiledQueryFragment{sql: `NOT (` + child.sql + `)`, args: child.args}, nil
	case query.ExpressionNear:
		return compileNearExpression(expression, field)
	case query.ExpressionTerm, query.ExpressionPhrase:
		return compileExpressionLeaf(expression, field)
	default:
		return compiledQueryFragment{}, compileExpressionError(syntax.Start, syntax.End, "unsupported expression operand")
	}
}

func compileAssociativeChildren(
	expression *query.ResolvedExpression, field string, kind query.ExpressionKind,
) ([]compiledQueryFragment, error) {
	var parts []compiledQueryFragment
	for _, child := range expression.Children {
		if child.Syntax.Kind == kind {
			nested, err := compileAssociativeChildren(child, field, kind)
			if err != nil {
				return nil, err
			}
			parts = append(parts, nested...)
			continue
		}
		part, err := compileResolvedExpression(child, field)
		if err != nil {
			return nil, err
		}
		parts = append(parts, part)
	}
	return parts, nil
}

func compileExpressionLeaf(expression *query.ResolvedExpression, field string) (compiledQueryFragment, error) {
	syntax := expression.Syntax
	if syntax.Value == "" {
		return compiledQueryFragment{}, compileExpressionError(syntax.Start, syntax.End, "field operand cannot be empty")
	}
	switch field {
	case "":
		return compileLexicalPredicate(quoteFTSOperand(syntax.Value, syntax.Prefix), true), nil
	case compiledNameField:
		return compileLexicalPredicate(quoteFTSOperand(syntax.Value, syntax.Prefix), false), nil
	case "path":
		if syntax.Prefix {
			return compiledQueryFragment{}, compileExpressionError(syntax.Start, syntax.End, "path operands cannot use prefix matching")
		}
		if err := query.ValidateTextOperand(field, syntax.Value); err != nil {
			return compiledQueryFragment{}, compileExpressionError(syntax.Start, syntax.End, err.Error())
		}
		return compilePathPredicate(syntax.Value), nil
	case "tag":
		if expression.Dependency == nil {
			return compiledQueryFragment{}, errors.New("resolved tag operand lacks a dependency")
		}
		return compileTagPredicate(expression.Dependency.ID), nil
	case "collection":
		if expression.Dependency == nil {
			return compiledQueryFragment{}, errors.New("resolved collection operand lacks a dependency")
		}
		return compileCollectionPredicate(expression.Dependency.ID), nil
	case "saved":
		if expression.Saved == nil {
			return compiledQueryFragment{}, errors.New("resolved saved operand lacks its query")
		}
		return compileSavedPredicate(expression)
	case "mime", "extension", "media_family", "modified_after", "modified_before", "size_min", "size_max":
		if syntax.Prefix {
			return compiledQueryFragment{}, compileExpressionError(syntax.Start, syntax.End, "scalar operands cannot use prefix matching")
		}
		return compileScalarPredicate(field, syntax.Value, syntax.Start, syntax.End)
	case "text_coverage", "has_duplicates":
		return compiledQueryFragment{}, compileExpressionError(syntax.Start, syntax.End, field+" fields are not supported by bound queries")
	default:
		return compiledQueryFragment{}, compileExpressionError(syntax.Start, syntax.End, "unsupported expression field")
	}
}

func compileNearExpression(expression *query.ResolvedExpression, field string) (compiledQueryFragment, error) {
	syntax := expression.Syntax
	if field != "" && field != compiledNameField {
		return compiledQueryFragment{}, compileExpressionError(syntax.Start, syntax.End, "NEAR is supported only for text and name operands")
	}
	if len(expression.Children) != 2 {
		return compiledQueryFragment{}, errors.New("resolved NEAR expression has invalid children")
	}
	operands := make([]string, 2)
	for index, child := range expression.Children {
		kind := child.Syntax.Kind
		if kind != query.ExpressionTerm && kind != query.ExpressionPhrase || child.Syntax.Value == "" {
			return compiledQueryFragment{}, compileExpressionError(
				child.Syntax.Start, child.Syntax.End, "NEAR requires two direct term or phrase operands",
			)
		}
		operands[index] = quoteFTSOperand(child.Syntax.Value, child.Syntax.Prefix)
	}
	fts := `NEAR(` + operands[0] + ` ` + operands[1] + `,` + strconv.Itoa(syntax.Distance) + `)`
	return compileLexicalPredicate(fts, field == ""), nil
}

func compileSavedPredicate(expression *query.ResolvedExpression) (compiledQueryFragment, error) {
	nested, err := compileResolvedQuery(*expression.Saved)
	if err != nil {
		if expressionErr, ok := errors.AsType[*query.ExpressionError](err); ok {
			return compiledQueryFragment{}, compileExpressionError(
				expression.Syntax.Start, expression.Syntax.End, expressionErr.Message,
			)
		}
		return compiledQueryFragment{}, err
	}
	return nested, nil
}

func compileScalarPredicate(field, value string, start, end int) (compiledQueryFragment, error) {
	switch field {
	case "mime", "extension", "media_family":
		if err := query.ValidateTextOperand(field, value); err != nil {
			return compiledQueryFragment{}, compileExpressionError(start, end, err.Error())
		}
		switch field {
		case "mime":
			return compileMIMEPredicate(value), nil
		case "extension":
			return compileExtensionPredicate(value), nil
		default:
			return compileMediaFamilyPredicate(value), nil
		}
	case "modified_after", "modified_before":
		key, err := query.TimestampKey(value)
		if err != nil {
			return compiledQueryFragment{}, compileExpressionError(start, end, err.Error())
		}
		operator := `>=`
		if field == "modified_before" {
			operator = `<`
		}
		return compiledQueryFragment{
			sql: `n.modified_at ` + operator + ` ?`, args: []any{key},
		}, nil
	case "size_min", "size_max":
		bound, err := query.ParseSizeOperand(value)
		if err != nil {
			return compiledQueryFragment{}, compileExpressionError(start, end, err.Error())
		}
		return compileSizePredicate(field, bound), nil
	default:
		return compiledQueryFragment{}, compileExpressionError(start, end, "unsupported scalar field")
	}
}

func compileQueryFilters(filters query.Filters, start, end int) (compiledQueryFragment, error) {
	parts := []compiledQueryFragment{
		compileValuePredicates(filters.Paths, compilePathPredicate),
		negateCompiledFragment(compileValuePredicates(filters.ExcludePaths, compilePathPredicate)),
		compileValuePredicates(filters.CollectionIDs, compileCollectionPredicate),
		negateCompiledFragment(compileValuePredicates(filters.ExcludeCollectionIDs, compileCollectionPredicate)),
		compileValuePredicates(filters.TagIDs, compileTagPredicate),
		negateCompiledFragment(compileValuePredicates(filters.ExcludeTagIDs, compileTagPredicate)),
	}
	if filters.NoTags {
		parts = append(parts, compiledQueryFragment{sql: `NOT EXISTS (SELECT 1 FROM node_tags nt WHERE nt.node_id=n.id)`})
	}
	parts = append(parts,
		compileValuePredicates(filters.MediaFamilies, compileMediaFamilyPredicate),
		compileValuePredicates(filters.MIMETypes, compileMIMEPredicate),
		compileValuePredicates(filters.Extensions, compileExtensionPredicate),
	)
	for _, bound := range []struct{ field, value string }{
		{"modified_after", filters.ModifiedAfter},
		{"modified_before", filters.ModifiedBefore},
	} {
		if bound.value == "" {
			continue
		}
		part, err := compileScalarPredicate(bound.field, bound.value, start, end)
		if err != nil {
			return compiledQueryFragment{}, err
		}
		parts = append(parts, part)
	}
	if filters.SizeMin > 0 {
		parts = append(parts, compileSizePredicate("size_min", filters.SizeMin))
	}
	if filters.SizeMax != nil {
		parts = append(parts, compileSizePredicate("size_max", *filters.SizeMax))
	}
	return joinCompiledFragments(parts, ` AND `), nil
}

func compileSizePredicate(field string, bound int64) compiledQueryFragment {
	operator := `>=`
	if field == "size_max" {
		operator = `<=`
	}
	return compiledQueryFragment{sql: `cv.size ` + operator + ` ?`, args: []any{bound}}
}

func compileValuePredicates(
	values []string, compile func(string) compiledQueryFragment,
) compiledQueryFragment {
	parts := make([]compiledQueryFragment, 0, len(values))
	for _, value := range values {
		parts = append(parts, compile(value))
	}
	return joinCompiledFragments(parts, ` OR `)
}

func compilePathPredicate(path string) compiledQueryFragment {
	if path == "/" {
		return trueCompiledFragment()
	}
	return compiledQueryFragment{
		sql: `EXISTS (
			WITH RECURSIVE query_ancestry(id,parent_id,path) AS (
				SELECT n.id,n.parent_id,n.name
				UNION ALL
				SELECT parent.id,parent.parent_id,
					CASE WHEN parent.parent_id IS NULL THEN '/' || child.path
					ELSE parent.name || '/' || child.path END
				FROM nodes parent JOIN query_ancestry child ON parent.id=child.parent_id
				WHERE parent.trashed_at IS NULL
			)
			SELECT 1 FROM query_ancestry
			WHERE parent_id IS NULL
			  AND (path=? OR substr(path,1,length(?)+1)=? || '/')
		)`,
		args: []any{path, path, path},
	}
}

func compileTagPredicate(id string) compiledQueryFragment {
	return compiledQueryFragment{
		sql:  `EXISTS (SELECT 1 FROM node_tags nt WHERE nt.node_id=n.id AND nt.tag_id=?)`,
		args: []any{id},
	}
}

func compileCollectionPredicate(id string) compiledQueryFragment {
	return compiledQueryFragment{
		sql: `EXISTS (WITH ` + CollectionMembershipCTE + `
			SELECT 1 FROM collection_members cm WHERE cm.node_id=n.id AND cm.ingest_id=?)`,
		args: []any{id},
	}
}

func compileMIMEPredicate(value string) compiledQueryFragment {
	return compiledQueryFragment{
		sql: `lower(trim(CASE WHEN instr(cv.mime_type,';')=0 THEN cv.mime_type
			ELSE substr(cv.mime_type,1,instr(cv.mime_type,';')-1) END))=?`,
		args: []any{value},
	}
}

func compileExtensionPredicate(value string) compiledQueryFragment {
	escaped := strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`).Replace(value)
	// Require a basename before the extension dot: .pdf has no extension.
	return compiledQueryFragment{
		sql: `lower(n.name) LIKE ? ESCAPE '\'`, args: []any{`_%.` + escaped},
	}
}

func compileMediaFamilyPredicate(value string) compiledQueryFragment {
	return compiledQueryFragment{
		sql: `docbank_query_media_family_v1(COALESCE(cv.mime_type,''),n.name)=?`, args: []any{value},
	}
}

func compileLexicalPredicate(fts string, includeContent bool) compiledQueryFragment {
	name := compiledQueryFragment{
		sql: `n.id IN (SELECT rowid FROM nodes_fts WHERE nodes_fts MATCH ?)`, args: []any{fts},
	}
	if !includeContent {
		return name
	}
	marker := compiledGenerationArgument{}
	content := compiledQueryFragment{
		sql: `(
			(?='' AND EXISTS (
				SELECT 1 FROM content_fts
				JOIN text_searchable_versions tsv ON tsv.version_id=cv.version_id
				WHERE content_fts.blob_hash=cv.blob_hash AND content_fts MATCH ?
			))
			OR (?<>'' AND EXISTS (
				SELECT 1 FROM rendition_lexical_fts
				JOIN rendition_attachments a ON a.build_id=rendition_lexical_fts.build_id
				JOIN rendition_heads rh
				  ON rh.content_version_id=a.content_version_id
				 AND rh.profile_fingerprint=a.profile_fingerprint
				 AND rh.attachment_id=a.attachment_id
				WHERE a.content_version_id=cv.version_id
				  AND rendition_lexical_fts MATCH ?
				  AND EXISTS (
					SELECT 1 FROM rendition_lexical_generation_builds gb
					WHERE gb.generation_id=? AND gb.build_id=rendition_lexical_fts.build_id
				  )
			))
		)`,
		args: []any{marker, fts, marker, fts, marker},
	}
	return joinCompiledFragments([]compiledQueryFragment{name, content}, ` OR `)
}

func quoteFTSOperand(value string, prefix bool) string {
	quoted := `"` + strings.ReplaceAll(value, `"`, `""`) + `"`
	if prefix {
		quoted += `*`
	}
	return quoted
}

func negateCompiledFragment(fragment compiledQueryFragment) compiledQueryFragment {
	if fragment.sql == "" {
		return compiledQueryFragment{}
	}
	return compiledQueryFragment{sql: `NOT (` + fragment.sql + `)`, args: fragment.args}
}

func trueCompiledFragment() compiledQueryFragment {
	return compiledQueryFragment{sql: `1`}
}

func joinCompiledFragments(parts []compiledQueryFragment, operator string) compiledQueryFragment {
	var sqlParts []string
	var args []any
	for _, part := range parts {
		if part.sql == "" {
			continue
		}
		sqlParts = append(sqlParts, `(`+part.sql+`)`)
		args = append(args, part.args...)
	}
	return compiledQueryFragment{sql: strings.Join(sqlParts, operator), args: args}
}

func compileExpressionError(start, end int, message string) *query.ExpressionError {
	return &query.ExpressionError{Offset: start, End: end, Message: message}
}
