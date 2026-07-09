package rules

import (
	"fmt"
	"regexp"
	"time"
)

// This file is a hand-written recursive-descent parser: one method per grammar
// production, ordered from lowest to highest precedence. Keeping the productions
// literal (parseOrExpr → parseAndExpr → … → parsePrimary) is the point — the
// call graph is the grammar, so the operator precedence is legible instead of
// hidden in a table.

// maxDepth bounds how deeply productions may nest before the parser gives up.
// Recursion happens through parentheses, function arguments and chained unary
// minus; without a cap a pathological input like "((((…))))" would exhaust the
// goroutine stack.
const maxDepth = 64

// maxPatternLen caps a label matcher's regex. RE2 has no catastrophic
// backtracking, but an enormous pattern still costs memory to compile and hold.
const maxPatternLen = 512

// Parse compiles an alerting expression into an AST plus the optional trailing
// `for <duration>` clause (zero when absent). It is the single entry point the
// rule model calls.
func Parse(input string) (Expression, time.Duration, error) {
	if len(input) > maxInputLen {
		return nil, 0, fmt.Errorf("parse error: expression exceeds %d bytes", maxInputLen)
	}
	tokens, err := lex(input)
	if err != nil {
		return nil, 0, err
	}
	p := &parser{tokens: tokens}
	return p.parseRoot()
}

type parser struct {
	tokens []token
	pos    int
	depth  int
}

func (p *parser) cur() token { return p.tokens[p.pos] }

// peekNext returns the token after the cursor; it never runs off the end
// because lex always appends a terminal EOF.
func (p *parser) peekNext() token {
	if p.pos+1 < len(p.tokens) {
		return p.tokens[p.pos+1]
	}
	return p.tokens[len(p.tokens)-1]
}

// advance returns the current token and moves on, clamping at EOF so a buggy
// production can never index past the slice.
func (p *parser) advance() token {
	t := p.tokens[p.pos]
	if t.typ != tokEOF {
		p.pos++
	}
	return t
}

func (p *parser) enter() error {
	p.depth++
	if p.depth > maxDepth {
		return perrorf(p.cur().pos, "expression nested too deeply (max depth %d)", maxDepth)
	}
	return nil
}

func (p *parser) leave() { p.depth-- }

// desc renders a token for an error message: its literal text, or a friendly
// name for end of input.
func desc(t token) string {
	if t.typ == tokEOF {
		return "end of input"
	}
	return fmt.Sprintf("%q", t.val)
}

// root := orExpr ( "for" DURATION )?
func (p *parser) parseRoot() (Expression, time.Duration, error) {
	expr, err := p.parseOrExpr()
	if err != nil {
		return nil, 0, err
	}
	var forDur time.Duration
	if p.isKeyword("for") {
		p.advance()
		t := p.cur()
		if t.typ != tokDuration {
			return nil, 0, perrorf(t.pos, "expected a duration after \"for\" but found %s", desc(t))
		}
		forDur = t.dur
		p.advance()
	}
	if p.cur().typ != tokEOF {
		return nil, 0, perrorf(p.cur().pos, "expected end of input but found %s", desc(p.cur()))
	}
	return expr, forDur, nil
}

// isKeyword reports whether the cursor is a bare identifier with the given text;
// keywords (for, and, or, by, …) are contextual, not reserved by the lexer.
func (p *parser) isKeyword(kw string) bool {
	return p.cur().typ == tokIdent && p.cur().val == kw
}

// orExpr := andExpr ( "or" andExpr )*
func (p *parser) parseOrExpr() (Expression, error) {
	if err := p.enter(); err != nil {
		return nil, err
	}
	defer p.leave()
	left, err := p.parseAndExpr()
	if err != nil {
		return nil, err
	}
	for p.isKeyword("or") {
		op := p.advance().val
		right, err := p.parseAndExpr()
		if err != nil {
			return nil, err
		}
		left = &BinaryOp{Op: op, Left: left, Right: right}
	}
	return left, nil
}

// andExpr := comparison ( ("and"|"unless") comparison )*
//
// `unless` binds as tightly as `and`, and tighter than `or` — as in PromQL.
// Parsing it alongside `or` instead would read `a or b unless c` as
// `(a or b) unless c` rather than `a or (b unless c)`, silently changing which
// alerts fire.
func (p *parser) parseAndExpr() (Expression, error) {
	left, err := p.parseComparison()
	if err != nil {
		return nil, err
	}
	for p.isKeyword("and") || p.isKeyword("unless") {
		op := p.advance().val
		right, err := p.parseComparison()
		if err != nil {
			return nil, err
		}
		left = &BinaryOp{Op: op, Left: left, Right: right}
	}
	return left, nil
}

// comparison := addition ( ("=="|"!="|">"|"<"|">="|"<=") addition )?
//
// Comparisons are non-associative: `a > b > c` is a parse error, which surfaces
// as "expected end of input" on the second operator.
func (p *parser) parseComparison() (Expression, error) {
	left, err := p.parseAddition()
	if err != nil {
		return nil, err
	}
	if p.cur().typ == tokOp && isComparisonOp(p.cur().val) {
		op := p.advance().val
		right, err := p.parseAddition()
		if err != nil {
			return nil, err
		}
		left = &BinaryOp{Op: op, Left: left, Right: right}
	}
	return left, nil
}

// addition := multiplication ( ("+"|"-") multiplication )*
func (p *parser) parseAddition() (Expression, error) {
	left, err := p.parseMultiplication()
	if err != nil {
		return nil, err
	}
	for p.cur().typ == tokOp && (p.cur().val == "+" || p.cur().val == "-") {
		op := p.advance().val
		right, err := p.parseMultiplication()
		if err != nil {
			return nil, err
		}
		left = &BinaryOp{Op: op, Left: left, Right: right}
	}
	return left, nil
}

// multiplication := unary ( ("*"|"/"|"%") unary )*
func (p *parser) parseMultiplication() (Expression, error) {
	left, err := p.parseUnary()
	if err != nil {
		return nil, err
	}
	for p.cur().typ == tokOp && (p.cur().val == "*" || p.cur().val == "/" || p.cur().val == "%") {
		op := p.advance().val
		right, err := p.parseUnary()
		if err != nil {
			return nil, err
		}
		left = &BinaryOp{Op: op, Left: left, Right: right}
	}
	return left, nil
}

// unary := "-" unary | primary
func (p *parser) parseUnary() (Expression, error) {
	if p.cur().typ == tokOp && p.cur().val == "-" {
		if err := p.enter(); err != nil {
			return nil, err
		}
		defer p.leave()
		p.advance()
		operand, err := p.parseUnary()
		if err != nil {
			return nil, err
		}
		return &UnaryOp{Op: "-", Operand: operand}, nil
	}
	return p.parsePrimary()
}

// primary := NUMBER | "(" orExpr ")" | aggregation | funcCall | metricRef
func (p *parser) parsePrimary() (Expression, error) {
	t := p.cur()
	switch t.typ {
	case tokNumber:
		p.advance()
		return &NumberLiteral{Value: t.num}, nil
	case tokLParen:
		p.advance()
		e, err := p.parseOrExpr()
		if err != nil {
			return nil, err
		}
		if p.cur().typ != tokRParen {
			return nil, perrorf(p.cur().pos, "expected \")\" but found %s", desc(p.cur()))
		}
		p.advance()
		return e, nil
	case tokIdent:
		switch {
		case aggregationOps[t.val]:
			return p.parseAggregation()
		case p.peekNext().typ == tokLParen:
			return p.parseFunctionCall()
		default:
			return p.parseMetricRef()
		}
	default:
		return nil, perrorf(t.pos, "expected an expression but found %s", desc(t))
	}
}

// aggregation := AGGOP ( ("by"|"without") grouping )? "(" orExpr ")" ( ("by"|"without") grouping )?
func (p *parser) parseAggregation() (Expression, error) {
	op := p.advance().val
	var grouping []string
	without := false
	hasClause := false
	if p.isKeyword("by") || p.isKeyword("without") {
		without = p.advance().val == "without"
		g, err := p.parseGrouping()
		if err != nil {
			return nil, err
		}
		grouping, hasClause = g, true
	}
	if p.cur().typ != tokLParen {
		return nil, perrorf(p.cur().pos, "expected \"(\" after %s but found %s", op, desc(p.cur()))
	}
	p.advance()
	operand, err := p.parseOrExpr()
	if err != nil {
		return nil, err
	}
	if p.cur().typ != tokRParen {
		return nil, perrorf(p.cur().pos, "expected \")\" but found %s", desc(p.cur()))
	}
	p.advance()
	// The trailing by/without form is only accepted when no leading clause was
	// given, so an aggregation never carries two grouping clauses.
	if !hasClause && (p.isKeyword("by") || p.isKeyword("without")) {
		without = p.advance().val == "without"
		g, err := p.parseGrouping()
		if err != nil {
			return nil, err
		}
		grouping = g
	}
	return &Aggregation{Op: op, Grouping: grouping, Without: without, Operand: operand}, nil
}

// grouping := "(" ( IDENT ("," IDENT)* )? ")"
func (p *parser) parseGrouping() ([]string, error) {
	if p.cur().typ != tokLParen {
		return nil, perrorf(p.cur().pos, "expected \"(\" to open a grouping clause but found %s", desc(p.cur()))
	}
	p.advance()
	var labels []string
	if p.cur().typ != tokRParen {
		for {
			if p.cur().typ != tokIdent {
				return nil, perrorf(p.cur().pos, "expected a label name but found %s", desc(p.cur()))
			}
			labels = append(labels, p.advance().val)
			if p.cur().typ == tokComma {
				p.advance()
				continue
			}
			break
		}
	}
	if p.cur().typ != tokRParen {
		return nil, perrorf(p.cur().pos, "expected \",\" or \")\" but found %s", desc(p.cur()))
	}
	p.advance()
	return labels, nil
}

// funcCall := IDENT "(" ( orExpr ("," orExpr)* )? ")"
func (p *parser) parseFunctionCall() (Expression, error) {
	nameTok := p.advance()
	if !isFunction(nameTok.val) {
		return nil, perrorf(nameTok.pos, "unknown function %q", nameTok.val)
	}
	p.advance() // consume "(" (peekNext guaranteed it)
	var args []Expression
	if p.cur().typ != tokRParen {
		for {
			a, err := p.parseOrExpr()
			if err != nil {
				return nil, err
			}
			args = append(args, a)
			if p.cur().typ == tokComma {
				p.advance()
				continue
			}
			break
		}
	}
	if p.cur().typ != tokRParen {
		return nil, perrorf(p.cur().pos, "expected \",\" or \")\" but found %s", desc(p.cur()))
	}
	p.advance()

	// Arity and argument shape are checked here rather than at evaluation time: a
	// rule with a malformed call must be rejected when it is created, not silently
	// accepted and then fail on every tick at 3am.
	name := nameTok.val
	if rangeFunctions[name] {
		if len(args) != 1 {
			return nil, perrorf(nameTok.pos, "%s expects exactly one argument, got %d", name, len(args))
		}
		if mr, ok := args[0].(*MetricRef); !ok || mr.Range == 0 {
			return nil, perrorf(nameTok.pos, "%s requires a range vector selector like metric[5m]", name)
		}
	} else if arity := instantFunctions[name]; len(args) != arity {
		return nil, perrorf(nameTok.pos, "%s expects %d argument(s), got %d", name, arity, len(args))
	}
	return &FunctionCall{Name: name, Args: args}, nil
}

// metricRef := IDENT ( "{" matcher ("," matcher)* "}" )? ( "[" DURATION "]" )?
func (p *parser) parseMetricRef() (Expression, error) {
	nameTok := p.advance()
	mr := &MetricRef{Name: nameTok.val}
	if p.cur().typ == tokLBrace {
		p.advance()
		var matchers []LabelMatcher
		if p.cur().typ != tokRBrace {
			for {
				m, err := p.parseMatcher()
				if err != nil {
					return nil, err
				}
				matchers = append(matchers, m)
				if p.cur().typ == tokComma {
					p.advance()
					continue
				}
				break
			}
		}
		if p.cur().typ != tokRBrace {
			return nil, perrorf(p.cur().pos, "expected \",\" or \"}\" but found %s", desc(p.cur()))
		}
		p.advance()
		mr.Matchers = matchers
	}
	if p.cur().typ == tokLBracket {
		p.advance()
		if p.cur().typ != tokDuration {
			return nil, perrorf(p.cur().pos, "expected a range duration like [5m] but found %s", desc(p.cur()))
		}
		// A zero or negative range would silently degrade the selector back to an
		// instant one, so reject it rather than surprise the rule's author.
		if d := p.cur().dur; d <= 0 {
			return nil, perrorf(p.cur().pos, "expected a range duration greater than zero, got %s", p.cur().val)
		}
		mr.Range = p.advance().dur
		if p.cur().typ != tokRBracket {
			return nil, perrorf(p.cur().pos, "expected \"]\" but found %s", desc(p.cur()))
		}
		p.advance()
	}
	return mr, nil
}

// matcher := IDENT ("="|"!="|"=~"|"!~") STRING
func (p *parser) parseMatcher() (LabelMatcher, error) {
	if p.cur().typ != tokIdent {
		return LabelMatcher{}, perrorf(p.cur().pos, "expected a label name but found %s", desc(p.cur()))
	}
	name := p.advance().val
	opTok := p.cur()
	if opTok.typ != tokOp || !isMatchOp(opTok.val) {
		return LabelMatcher{}, perrorf(opTok.pos, "expected a matcher operator (=, !=, =~, !~) but found %s", desc(opTok))
	}
	p.advance()
	valTok := p.cur()
	if valTok.typ != tokString {
		return LabelMatcher{}, perrorf(valTok.pos, "expected a quoted string but found %s", desc(valTok))
	}
	p.advance()
	lm := LabelMatcher{Name: name, Op: MatchOp(opTok.val), Value: valTok.val}
	if lm.Op == MatchRe || lm.Op == MatchNre {
		if len(valTok.val) > maxPatternLen {
			return LabelMatcher{}, perrorf(valTok.pos, "regex pattern exceeds %d bytes", maxPatternLen)
		}
		// Anchor the pattern so =~ means a full match, matching PromQL and
		// avoiding surprise substring hits.
		re, err := regexp.Compile("^(?:" + valTok.val + ")$")
		if err != nil {
			return LabelMatcher{}, perrorf(valTok.pos, "invalid regular expression %q: %v", valTok.val, err)
		}
		lm.re = re
	}
	return lm, nil
}

func isComparisonOp(op string) bool {
	switch op {
	case "==", "!=", ">", "<", ">=", "<=":
		return true
	}
	return false
}

func isMatchOp(op string) bool {
	switch op {
	case "=", "!=", "=~", "!~":
		return true
	}
	return false
}
