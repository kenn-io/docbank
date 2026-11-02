// Package sqlcheck reports CHECK constraints and enum types in SQL schema
// and migration text.
//
// A CHECK constraint bakes a validation rule into the schema: every change to
// the rule, such as a new allowed value for a status column, needs a migration
// that drops and recreates the constraint (and on SQLite, often the whole
// table). The same goes for CREATE TYPE ... AS ENUM. Rules belong in
// application code or in lookup tables, where they can change without a
// schema migration, so the package reports every occurrence.
//
// The package scans raw SQL (migration files) and, through Analyzer, SQL held
// in Go string literals.
package sqlcheck

import (
	"fmt"
	"go/ast"
	"go/token"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"golang.org/x/tools/go/analysis"
	"golang.org/x/tools/go/analysis/passes/inspect"
	"golang.org/x/tools/go/ast/inspector"
)

// Kind classifies what a Finding reports.
type Kind int

const (
	// CheckConstraint is a CHECK (...) constraint.
	CheckConstraint Kind = iota
	// EnumType is a CREATE TYPE ... AS ENUM (...) declaration.
	EnumType
)

// Finding is one CHECK constraint or enum type in a SQL source.
type Finding struct {
	// Kind says whether the finding is a CHECK constraint or an enum type.
	Kind Kind
	// Offset is the byte offset of the CHECK or CREATE TYPE keyword.
	Offset int
	// Line and Column are 1-based positions of the keyword.
	Line, Column int
	// Name is the enum type name, or the first column the constraint
	// mentions (empty when none can be identified).
	Name string
	// Expr is the constraint expression or the enum value list as written.
	Expr string
}

// Message renders the diagnostic for a finding.
func (f Finding) Message() string {
	if f.Kind == EnumType {
		return fmt.Sprintf("enum type %s locks its allowed values into the schema; every new value then needs a migration to alter the type. Store the column as text and validate the set in application code or a lookup table", f.Name)
	}
	subject := "CHECK constraint"
	if f.Name != "" {
		subject += " on " + f.Name
	}
	return subject + " locks a validation rule into the schema; every change to the rule then needs a schema migration to rewrite the constraint. Validate in application code or keep allowed values in a lookup table"
}

var (
	checkKeyword  = regexp.MustCompile(`(?i)\bCHECK\s*\(`)
	enumKeyword   = regexp.MustCompile(`(?i)\bCREATE\s+TYPE\s+(` + name + `)\s+AS\s+ENUM\s*\(`)
	sqlDDLKeyword = regexp.MustCompile(`(?i)\b(?:CREATE\s+(?:(?:TEMP|TEMPORARY)\s+)?(?:TABLE|TYPE|DOMAIN)|ALTER\s+(?:TABLE|TYPE|DOMAIN))\b`)
	whitespace    = regexp.MustCompile(`\s+`)

	// name is a possibly quoted or qualified identifier.
	identifier = `(?:"(?:[^"]|"")+"|` + "`(?:[^`]|``)+`" + `|\[(?:[^\]]|\]\])+\]|[A-Za-z_]\w*)`
	name       = identifier + `(?:\.` + identifier + `)*`
	// firstColumn finds the first bare identifier in a constraint expression
	// that is not a SQL keyword or a function call.
	firstColumn = regexp.MustCompile(`(?i)(?:^|[\s(])(` + name + `)(?:$|[\s),=<>!])`)

	keywords = map[string]bool{
		"not": true, "null": true, "and": true, "or": true, "in": true, "is": true,
		"between": true, "like": true, "true": true, "false": true, "any": true,
		"all": true, "array": true, "select": true, "exists": true, "case": true,
		"when": true, "then": true, "else": true, "end": true, "glob": true,
		"escape": true, "some": true,
	}
)

// Scan returns every CHECK constraint and enum type in src, in source order.
// Keywords inside SQL comments or string literals are ignored.
func Scan(src string) []Finding {
	var findings []Finding
	masked := maskSQL(src, true)
	for _, loc := range checkKeyword.FindAllStringIndex(masked, -1) {
		open := loc[1] - 1
		end := matchParen(masked, open)
		if end < 0 {
			continue
		}
		expr := strings.TrimSpace(src[open+1 : end])
		line, col := position(src, loc[0])
		findings = append(findings, Finding{Kind: CheckConstraint, Offset: loc[0], Line: line, Column: col, Name: columnName(expr), Expr: expr})
	}
	for _, m := range enumKeyword.FindAllStringSubmatchIndex(maskSQL(src, false), -1) {
		open := m[1] - 1
		end := matchParen(masked, open)
		if end < 0 {
			continue
		}
		line, col := position(src, m[0])
		findings = append(findings, Finding{Kind: EnumType, Offset: m[0], Line: line, Column: col, Name: src[m[2]:m[3]], Expr: strings.TrimSpace(src[open+1 : end])})
	}
	sort.Slice(findings, func(i, j int) bool { return findings[i].Offset < findings[j].Offset })
	return findings
}

// columnName returns the first column an expression mentions, for the
// diagnostic. Function names and keywords are skipped.
func columnName(expr string) string {
	normalized := whitespace.ReplaceAllString(maskSQL(expr, false), " ")
	for _, m := range firstColumn.FindAllStringSubmatchIndex(normalized, -1) {
		candidate := normalized[m[2]:m[3]]
		if keywords[strings.ToLower(candidate)] {
			continue
		}
		if rest := strings.TrimLeft(normalized[m[3]:], " "); strings.HasPrefix(rest, "(") {
			continue // function call
		}
		return candidate
	}
	return ""
}

// maskSQL blanks comments and string literals with spaces. When
// maskIdentifiers is true, it also blanks quoted identifiers. Offsets and
// newlines are preserved, so positions map back to src.
func maskSQL(src string, maskIdentifiers bool) string {
	out := []byte(src)
	blank := func(from, to int) {
		for i := from; i < to && i < len(out); i++ {
			if out[i] != '\n' {
				out[i] = ' '
			}
		}
	}
	for i := 0; i < len(src); {
		switch {
		case src[i] == '\'':
			end := quotedEnd(src, i, '\'')
			blank(i, end)
			i = end
		case src[i] == '$':
			delim, ok := dollarQuoteDelimiter(src, i)
			if !ok {
				i++
				continue
			}
			start := i + len(delim)
			end := strings.Index(src[start:], delim)
			if end < 0 {
				end = len(src)
			} else {
				end += start + len(delim)
			}
			blank(i, end)
			i = end
		case strings.HasPrefix(src[i:], "--"):
			end := strings.IndexByte(src[i:], '\n')
			if end < 0 {
				end = len(src) - i
			}
			blank(i, i+end)
			i += end
		case strings.HasPrefix(src[i:], "/*"):
			end, depth := i+2, 1
			for end < len(src) && depth > 0 {
				switch {
				case strings.HasPrefix(src[end:], "/*"):
					depth++
					end += 2
				case strings.HasPrefix(src[end:], "*/"):
					depth--
					end += 2
				default:
					end++
				}
			}
			blank(i, end)
			i = end
		case src[i] == '"' || src[i] == '`' || src[i] == '[':
			closing := src[i]
			if closing == '[' {
				closing = ']'
			}
			end := quotedEnd(src, i, closing)
			if maskIdentifiers {
				blank(i, end)
			}
			i = end
		default:
			i++
		}
	}
	return string(out)
}

func quotedEnd(src string, start int, closing byte) int {
	for i := start + 1; i < len(src); i++ {
		if src[i] != closing {
			continue
		}
		if i+1 < len(src) && src[i+1] == closing {
			i++
			continue
		}
		return i + 1
	}
	return len(src)
}

func dollarQuoteDelimiter(src string, start int) (string, bool) {
	if start+1 >= len(src) {
		return "", false
	}
	if src[start+1] == '$' {
		return "$$", true
	}
	if c := src[start+1]; !isASCIIAlpha(c) && c != '_' {
		return "", false
	}
	for i := start + 2; i < len(src); i++ {
		switch c := src[i]; {
		case c == '$':
			return src[start : i+1], true
		case isASCIIAlpha(c) || c == '_' || c >= '0' && c <= '9':
		default:
			return "", false
		}
	}
	return "", false
}

func isASCIIAlpha(c byte) bool {
	return c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z'
}

// matchParen returns the index of the parenthesis closing the one at open in
// SQL whose comments, strings, and quoted identifiers have already been
// masked, or -1.
func matchParen(src string, open int) int {
	depth := 0
	for i := open; i < len(src); i++ {
		switch src[i] {
		case '(':
			depth++
		case ')':
			depth--
			if depth == 0 {
				return i
			}
		}
	}
	return -1
}

func position(src string, offset int) (line, column int) {
	line = 1 + strings.Count(src[:offset], "\n")
	column = offset - strings.LastIndexByte(src[:offset], '\n')
	return line, column
}

// Analyzer reports CHECK constraints and enum types inside Go string
// literals in non-test files, where embedded schema and migration SQL
// usually lives.
var Analyzer = &analysis.Analyzer{
	Name:     "sqlcheck",
	Doc:      "reports SQL CHECK constraints and enum types, which lock validation rules into the schema",
	Requires: []*analysis.Analyzer{inspect.Analyzer},
	Run:      run,
}

func run(pass *analysis.Pass) (any, error) {
	inspect := pass.ResultOf[inspect.Analyzer].(*inspector.Inspector)
	inspect.Preorder([]ast.Node{(*ast.BasicLit)(nil)}, func(n ast.Node) {
		lit := n.(*ast.BasicLit)
		if lit.Kind != token.STRING || strings.HasSuffix(pass.Fset.Position(lit.Pos()).Filename, "_test.go") {
			return
		}
		value, err := strconv.Unquote(lit.Value)
		if err != nil {
			return
		}
		if !sqlDDLKeyword.MatchString(maskSQL(value, true)) {
			return
		}
		for _, f := range Scan(value) {
			pos := lit.Pos()
			if lit.Value[0] == '`' {
				// Raw strings map byte offsets directly; escapes in
				// interpreted strings do not, so those report at the literal.
				pos += token.Pos(1 + f.Offset)
			}
			pass.Reportf(pos, "%s", f.Message())
		}
	})
	return nil, nil
}
