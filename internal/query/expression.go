package query

import (
	"fmt"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"
)

const (
	maxExpressionNodes = 512
	maxExpressionDepth = 32
)

// ExpressionKind identifies one node in a parsed search expression.
type ExpressionKind string

const (
	ExpressionAll    ExpressionKind = "all"
	ExpressionTerm   ExpressionKind = "term"
	ExpressionPhrase ExpressionKind = "phrase"
	ExpressionAnd    ExpressionKind = "and"
	ExpressionOr     ExpressionKind = "or"
	ExpressionNot    ExpressionKind = "not"
	ExpressionNear   ExpressionKind = "near"
	ExpressionField  ExpressionKind = "field"
)

// Expression is a bounded syntax tree whose spans refer to the original text.
type Expression struct {
	Kind     ExpressionKind
	Start    int
	End      int
	Value    string
	Prefix   bool
	Field    string
	Distance int
	Children []*Expression
}

// ExpressionError reports a half-open byte span in the original text.
type ExpressionError struct {
	Offset  int
	End     int
	Message string
}

func (e *ExpressionError) Error() string {
	return fmt.Sprintf("expression bytes %d:%d: %s", e.Offset, e.End, e.Message)
}

// ParseExpression parses one bounded simple or advanced search expression.
func ParseExpression(text, syntax string) (*Expression, error) {
	if syntax != "simple" && syntax != "advanced" {
		return nil, expressionError(0, 0, "unknown syntax mode")
	}
	if !utf8.ValidString(text) {
		return nil, expressionError(0, len(text), "expression is not valid UTF-8")
	}
	if utf8.RuneCountInString(text) > maxTextRunes {
		return nil, expressionError(0, len(text), "expression exceeds 8192 Unicode code points")
	}
	if syntax == "simple" {
		return parseSimpleExpression(text)
	}

	parser := expressionParser{lexer: expressionLexer{text: text}, textLen: len(text)}
	if err := parser.advance(); err != nil {
		return nil, err
	}
	if parser.current.kind == expressionTokenEOF {
		return &Expression{Kind: ExpressionAll, Start: 0, End: len(text)}, nil
	}
	expr, err := parser.parseOr(0)
	if err != nil {
		return nil, err
	}
	if parser.current.kind != expressionTokenEOF {
		return nil, expressionError(parser.current.start, parser.current.end, "unexpected token")
	}
	return expr, nil
}

func parseSimpleExpression(text string) (*Expression, error) {
	var root *Expression
	nodes := 0
	termStart := -1
	appendTerm := func(end int) error {
		if nodes >= maxExpressionNodes {
			return expressionError(termStart, end, "expression exceeds 512 AST nodes")
		}
		term := &Expression{
			Kind: ExpressionTerm, Start: termStart, End: end,
			Value: text[termStart:end], Prefix: true,
		}
		nodes++
		if root == nil {
			root = term
			return nil
		}
		if nodes >= maxExpressionNodes {
			return expressionError(root.Start, end, "expression exceeds 512 AST nodes")
		}
		root = &Expression{
			Kind: ExpressionAnd, Start: root.Start, End: end,
			Children: []*Expression{root, term},
		}
		nodes++
		return nil
	}

	for offset := 0; offset < len(text); {
		r, size := utf8.DecodeRuneInString(text[offset:])
		if unicode.IsSpace(r) {
			if termStart >= 0 {
				if err := appendTerm(offset); err != nil {
					return nil, err
				}
				termStart = -1
			}
		} else if termStart < 0 {
			termStart = offset
		}
		offset += size
	}
	if termStart >= 0 {
		if err := appendTerm(len(text)); err != nil {
			return nil, err
		}
	}
	if root == nil {
		return &Expression{Kind: ExpressionAll, Start: 0, End: len(text)}, nil
	}
	return root, nil
}

type expressionTokenKind uint8

const (
	expressionTokenEOF expressionTokenKind = iota
	expressionTokenWord
	expressionTokenPhrase
	expressionTokenLeftParen
	expressionTokenRightParen
	expressionTokenColon
	expressionTokenAnd
	expressionTokenOr
	expressionTokenNot
	expressionTokenNear
)

type expressionToken struct {
	kind     expressionTokenKind
	start    int
	end      int
	value    string
	prefix   bool
	distance int
}

type expressionLexer struct {
	text string
	pos  int
}

func (lexer *expressionLexer) next() (expressionToken, error) {
	for lexer.pos < len(lexer.text) {
		r, size := utf8.DecodeRuneInString(lexer.text[lexer.pos:])
		if !unicode.IsSpace(r) {
			break
		}
		lexer.pos += size
	}
	if lexer.pos == len(lexer.text) {
		return expressionToken{kind: expressionTokenEOF, start: lexer.pos, end: lexer.pos}, nil
	}

	start := lexer.pos
	switch lexer.text[lexer.pos] {
	case '(':
		lexer.pos++
		return expressionToken{kind: expressionTokenLeftParen, start: start, end: lexer.pos}, nil
	case ')':
		lexer.pos++
		return expressionToken{kind: expressionTokenRightParen, start: start, end: lexer.pos}, nil
	case ':':
		lexer.pos++
		return expressionToken{kind: expressionTokenColon, start: start, end: lexer.pos}, nil
	case '"':
		return lexer.phrase()
	case '*':
		lexer.pos++
		return expressionToken{}, expressionError(start, lexer.pos, "star requires a preceding term or phrase")
	default:
		return lexer.word()
	}
}

func (lexer *expressionLexer) phrase() (expressionToken, error) {
	start := lexer.pos
	lexer.pos++
	var value strings.Builder
	for lexer.pos < len(lexer.text) {
		r, size := utf8.DecodeRuneInString(lexer.text[lexer.pos:])
		if r == '"' {
			lexer.pos += size
			prefix := false
			if lexer.pos < len(lexer.text) && lexer.text[lexer.pos] == '*' {
				prefix = true
				lexer.pos++
				if lexer.pos < len(lexer.text) && lexer.text[lexer.pos] == '*' {
					return expressionToken{}, expressionError(lexer.pos, lexer.pos+1, "repeated star is invalid")
				}
				if !lexer.atTokenBoundary() {
					_, size := utf8.DecodeRuneInString(lexer.text[lexer.pos:])
					return expressionToken{}, expressionError(lexer.pos, lexer.pos+size, "star must be a trailing suffix")
				}
			}
			return expressionToken{
				kind: expressionTokenPhrase, start: start, end: lexer.pos,
				value: value.String(), prefix: prefix,
			}, nil
		}
		if r == '\\' {
			escapeStart := lexer.pos
			lexer.pos += size
			if lexer.pos == len(lexer.text) {
				return expressionToken{}, expressionError(escapeStart, lexer.pos, "escape requires a following code point")
			}
			r, size = utf8.DecodeRuneInString(lexer.text[lexer.pos:])
		}
		value.WriteRune(r)
		lexer.pos += size
	}
	return expressionToken{}, expressionError(start, len(lexer.text), "unclosed quoted phrase")
}

func (lexer *expressionLexer) word() (expressionToken, error) {
	start := lexer.pos
	escaped := false
	var value strings.Builder
	for lexer.pos < len(lexer.text) {
		r, size := utf8.DecodeRuneInString(lexer.text[lexer.pos:])
		if unicode.IsSpace(r) || isExpressionDelimiter(r) {
			break
		}
		if r == '\\' {
			escaped = true
			escapeStart := lexer.pos
			lexer.pos += size
			if lexer.pos == len(lexer.text) {
				return expressionToken{}, expressionError(escapeStart, lexer.pos, "escape requires a following code point")
			}
			r, size = utf8.DecodeRuneInString(lexer.text[lexer.pos:])
			value.WriteRune(r)
			lexer.pos += size
			continue
		}
		if r == '*' {
			if value.Len() == 0 {
				lexer.pos += size
				return expressionToken{}, expressionError(start, lexer.pos, "star requires a preceding term")
			}
			lexer.pos += size
			if !lexer.atTokenBoundary() {
				end := min(lexer.pos+1, len(lexer.text))
				return expressionToken{}, expressionError(lexer.pos, end, "star must be a single trailing suffix")
			}
			return expressionToken{
				kind: expressionTokenWord, start: start, end: lexer.pos,
				value: value.String(), prefix: true,
			}, nil
		}
		value.WriteRune(r)
		lexer.pos += size
	}
	if value.Len() == 0 {
		return expressionToken{}, expressionError(start, lexer.pos, "term is empty")
	}
	token := expressionToken{
		kind: expressionTokenWord, start: start, end: lexer.pos, value: value.String(),
	}
	if escaped {
		return token, nil
	}
	switch token.value {
	case "AND":
		token.kind = expressionTokenAnd
	case "OR":
		token.kind = expressionTokenOr
	case "NOT":
		token.kind = expressionTokenNot
	case "NEAR":
		token.kind = expressionTokenNear
		token.distance = 10
	default:
		if after, ok := strings.CutPrefix(token.value, "NEAR/"); ok {
			distance, err := strconv.Atoi(after)
			if err != nil || strings.Trim(after, "0123456789") != "" || distance > 1000 {
				return expressionToken{}, expressionError(start, lexer.pos, "NEAR distance must be an integer from 0 through 1000")
			}
			token.kind = expressionTokenNear
			token.distance = distance
		}
	}
	return token, nil
}

func (lexer *expressionLexer) atTokenBoundary() bool {
	if lexer.pos == len(lexer.text) {
		return true
	}
	r, _ := utf8.DecodeRuneInString(lexer.text[lexer.pos:])
	return unicode.IsSpace(r) || r == '(' || r == ')'
}

func isExpressionDelimiter(r rune) bool {
	return r == '(' || r == ')' || r == ':' || r == '"'
}

type expressionParser struct {
	lexer   expressionLexer
	current expressionToken
	textLen int
	nodes   int
}

func (parser *expressionParser) advance() error {
	next, err := parser.lexer.next()
	if err != nil {
		return err
	}
	parser.current = next
	return nil
}

func (parser *expressionParser) parseOr(depth int) (*Expression, error) {
	left, err := parser.parseAnd(depth)
	if err != nil {
		return nil, err
	}
	for parser.current.kind == expressionTokenOr {
		operator := parser.current
		if err := parser.advance(); err != nil {
			return nil, err
		}
		if !expressionTokenStartsOperand(parser.current.kind) {
			return nil, expressionError(operator.start, operator.end, "OR requires a right operand")
		}
		right, err := parser.parseAnd(depth)
		if err != nil {
			return nil, err
		}
		left, err = parser.node(&Expression{
			Kind: ExpressionOr, Start: left.Start, End: right.End,
			Children: []*Expression{left, right},
		})
		if err != nil {
			return nil, err
		}
	}
	return left, nil
}

func (parser *expressionParser) parseAnd(depth int) (*Expression, error) {
	left, err := parser.parseNear(depth)
	if err != nil {
		return nil, err
	}
	for {
		explicit := parser.current.kind == expressionTokenAnd
		if explicit {
			operator := parser.current
			if err := parser.advance(); err != nil {
				return nil, err
			}
			if !expressionTokenStartsOperand(parser.current.kind) {
				return nil, expressionError(operator.start, operator.end, "AND requires a right operand")
			}
		} else if !expressionTokenStartsOperand(parser.current.kind) {
			break
		}
		right, err := parser.parseNear(depth)
		if err != nil {
			return nil, err
		}
		left, err = parser.node(&Expression{
			Kind: ExpressionAnd, Start: left.Start, End: right.End,
			Children: []*Expression{left, right},
		})
		if err != nil {
			return nil, err
		}
	}
	return left, nil
}

func (parser *expressionParser) parseNear(depth int) (*Expression, error) {
	left, err := parser.parseUnary(depth)
	if err != nil {
		return nil, err
	}
	for parser.current.kind == expressionTokenNear {
		operator := parser.current
		if err := parser.advance(); err != nil {
			return nil, err
		}
		if !expressionTokenStartsOperand(parser.current.kind) {
			return nil, expressionError(operator.start, operator.end, "NEAR requires a right operand")
		}
		right, err := parser.parseUnary(depth)
		if err != nil {
			return nil, err
		}
		left, err = parser.node(&Expression{
			Kind: ExpressionNear, Start: left.Start, End: right.End,
			Distance: operator.distance, Children: []*Expression{left, right},
		})
		if err != nil {
			return nil, err
		}
	}
	return left, nil
}

func (parser *expressionParser) parseUnary(depth int) (*Expression, error) {
	if parser.current.kind != expressionTokenNot {
		return parser.parsePrimary(depth)
	}
	operator := parser.current
	if depth >= maxExpressionDepth {
		return nil, expressionError(operator.start, operator.end, "expression nesting exceeds 32")
	}
	if err := parser.advance(); err != nil {
		return nil, err
	}
	if !expressionTokenStartsOperand(parser.current.kind) {
		return nil, expressionError(operator.start, operator.end, "NOT requires an operand")
	}
	child, err := parser.parseUnary(depth + 1)
	if err != nil {
		return nil, err
	}
	return parser.node(&Expression{
		Kind: ExpressionNot, Start: operator.start, End: child.End,
		Children: []*Expression{child},
	})
}

func (parser *expressionParser) parsePrimary(depth int) (*Expression, error) {
	token := parser.current
	switch token.kind {
	case expressionTokenWord:
		if err := parser.advance(); err != nil {
			return nil, err
		}
		if parser.current.kind == expressionTokenColon {
			return parser.parseField(token, depth)
		}
		return parser.node(&Expression{
			Kind: ExpressionTerm, Start: token.start, End: token.end,
			Value: token.value, Prefix: token.prefix,
		})
	case expressionTokenPhrase:
		if err := parser.advance(); err != nil {
			return nil, err
		}
		return parser.node(&Expression{
			Kind: ExpressionPhrase, Start: token.start, End: token.end,
			Value: token.value, Prefix: token.prefix,
		})
	case expressionTokenLeftParen:
		return parser.parseGroup(depth)
	case expressionTokenEOF:
		return nil, expressionError(parser.textLen, parser.textLen, "missing operand")
	default:
		return nil, expressionError(token.start, token.end, "expected an operand")
	}
}

func (parser *expressionParser) parseGroup(depth int) (*Expression, error) {
	open := parser.current
	if depth >= maxExpressionDepth {
		return nil, expressionError(open.start, open.end, "expression nesting exceeds 32")
	}
	if err := parser.advance(); err != nil {
		return nil, err
	}
	if parser.current.kind == expressionTokenRightParen {
		return nil, expressionError(open.start, parser.current.end, "group cannot be empty")
	}
	child, err := parser.parseOr(depth + 1)
	if err != nil {
		return nil, err
	}
	if parser.current.kind != expressionTokenRightParen {
		return nil, expressionError(parser.current.start, parser.current.end, "group is missing a closing parenthesis")
	}
	child.Start = open.start
	child.End = parser.current.end
	if err := parser.advance(); err != nil {
		return nil, err
	}
	return child, nil
}

func (parser *expressionParser) parseField(name expressionToken, depth int) (*Expression, error) {
	colon := parser.current
	if name.prefix {
		return nil, expressionError(name.start, colon.end, "field name cannot have a prefix suffix")
	}
	if _, ok := expressionFields[name.value]; !ok {
		return nil, expressionError(name.start, colon.end, "unknown expression field")
	}
	if err := parser.advance(); err != nil {
		return nil, err
	}
	var child *Expression
	var err error
	switch parser.current.kind {
	case expressionTokenWord:
		operand := parser.current
		if err = parser.advance(); err == nil {
			child, err = parser.node(&Expression{
				Kind: ExpressionTerm, Start: operand.start, End: operand.end,
				Value: operand.value, Prefix: operand.prefix,
			})
		}
	case expressionTokenPhrase:
		operand := parser.current
		if err = parser.advance(); err == nil {
			child, err = parser.node(&Expression{
				Kind: ExpressionPhrase, Start: operand.start, End: operand.end,
				Value: operand.value, Prefix: operand.prefix,
			})
		}
	case expressionTokenLeftParen:
		child, err = parser.parseGroup(depth)
	default:
		return nil, expressionError(colon.start, colon.end, "field requires a term, phrase, or grouped expression")
	}
	if err != nil {
		return nil, err
	}
	return parser.node(&Expression{
		Kind: ExpressionField, Start: name.start, End: child.End, Field: name.value,
		Children: []*Expression{child},
	})
}

func (parser *expressionParser) node(expr *Expression) (*Expression, error) {
	if parser.nodes >= maxExpressionNodes {
		return nil, expressionError(expr.Start, expr.End, "expression exceeds 512 AST nodes")
	}
	parser.nodes++
	return expr, nil
}

func expressionTokenStartsOperand(kind expressionTokenKind) bool {
	return kind == expressionTokenWord || kind == expressionTokenPhrase ||
		kind == expressionTokenLeftParen || kind == expressionTokenNot
}

var expressionFields = map[string]struct{}{
	"name": {}, "path": {}, "tag": {}, "collection": {}, "saved": {},
	"mime": {}, "extension": {}, "media_family": {},
	"modified_after": {}, "modified_before": {}, "size_min": {}, "size_max": {},
	"text_coverage": {}, "has_duplicates": {},
}

func expressionError(start, end int, message string) *ExpressionError {
	return &ExpressionError{Offset: start, End: end, Message: message}
}
