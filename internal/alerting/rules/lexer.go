package rules

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// The lexer turns source text into a flat token slice terminated by a single
// EOF token, which lets the recursive-descent parser use simple index-based
// lookahead instead of a streaming cursor. Every token records the byte offset
// it starts at so parse errors can point at the offending position.

// maxInputLen caps the source length. Alerting expressions are short; a longer
// input is almost certainly hostile or a mistake, and the cap keeps the lexer
// and parser from doing unbounded work.
const maxInputLen = 4096

type tokenType int

const (
	tokEOF tokenType = iota
	tokNumber
	tokDuration
	tokIdent
	tokString
	tokLParen
	tokRParen
	tokLBrace
	tokRBrace
	tokLBracket
	tokRBracket
	tokComma
	tokOp
)

// token is one lexical unit. num is set for numbers, dur for durations, and val
// carries the literal text (the unescaped content for strings) used in error
// messages and by the parser.
type token struct {
	typ tokenType
	val string
	num float64
	dur time.Duration
	pos int
}

// lex scans the whole input up front. It fails fast on the first malformed
// token so callers never see a partially valid stream.
func lex(input string) ([]token, error) {
	l := &lexer{input: input}
	var toks []token
	for {
		t, err := l.next()
		if err != nil {
			return nil, err
		}
		toks = append(toks, t)
		if t.typ == tokEOF {
			return toks, nil
		}
	}
}

type lexer struct {
	input string
	pos   int
}

func (l *lexer) next() (token, error) {
	l.skipSpace()
	if l.pos >= len(l.input) {
		return token{typ: tokEOF, pos: l.pos}, nil
	}
	start := l.pos
	c := l.input[start]

	switch {
	case c == '(':
		l.pos++
		return token{typ: tokLParen, val: "(", pos: start}, nil
	case c == ')':
		l.pos++
		return token{typ: tokRParen, val: ")", pos: start}, nil
	case c == '{':
		l.pos++
		return token{typ: tokLBrace, val: "{", pos: start}, nil
	case c == '}':
		l.pos++
		return token{typ: tokRBrace, val: "}", pos: start}, nil
	case c == '[':
		l.pos++
		return token{typ: tokLBracket, val: "[", pos: start}, nil
	case c == ']':
		l.pos++
		return token{typ: tokRBracket, val: "]", pos: start}, nil
	case c == ',':
		l.pos++
		return token{typ: tokComma, val: ",", pos: start}, nil
	case c == '"':
		return l.lexString(start)
	case isDigit(c) || (c == '.' && start+1 < len(l.input) && isDigit(l.input[start+1])):
		return l.lexNumberOrDuration(start)
	case isIdentStart(c):
		return l.lexIdent(start)
	default:
		return l.lexOperator(start)
	}
}

func (l *lexer) skipSpace() {
	for l.pos < len(l.input) {
		switch l.input[l.pos] {
		case ' ', '\t', '\n', '\r':
			l.pos++
		default:
			return
		}
	}
}

func (l *lexer) lexOperator(start int) (token, error) {
	c := l.input[start]
	peek := func() byte {
		if start+1 < len(l.input) {
			return l.input[start+1]
		}
		return 0
	}
	switch c {
	case '=':
		switch peek() {
		case '~':
			l.pos += 2
			return token{typ: tokOp, val: "=~", pos: start}, nil
		case '=':
			l.pos += 2
			return token{typ: tokOp, val: "==", pos: start}, nil
		default:
			l.pos++
			return token{typ: tokOp, val: "=", pos: start}, nil
		}
	case '!':
		switch peek() {
		case '=':
			l.pos += 2
			return token{typ: tokOp, val: "!=", pos: start}, nil
		case '~':
			l.pos += 2
			return token{typ: tokOp, val: "!~", pos: start}, nil
		default:
			return token{}, perrorf(start, "unexpected %q (did you mean != or !~?)", "!")
		}
	case '>':
		if peek() == '=' {
			l.pos += 2
			return token{typ: tokOp, val: ">=", pos: start}, nil
		}
		l.pos++
		return token{typ: tokOp, val: ">", pos: start}, nil
	case '<':
		if peek() == '=' {
			l.pos += 2
			return token{typ: tokOp, val: "<=", pos: start}, nil
		}
		l.pos++
		return token{typ: tokOp, val: "<", pos: start}, nil
	case '+', '-', '*', '/', '%':
		l.pos++
		return token{typ: tokOp, val: string(c), pos: start}, nil
	default:
		return token{}, perrorf(start, "unexpected character %q", string(c))
	}
}

func (l *lexer) lexString(start int) (token, error) {
	var b strings.Builder
	i := start + 1
	for i < len(l.input) {
		c := l.input[i]
		switch c {
		case '\\':
			i++
			if i >= len(l.input) {
				return token{}, perrorf(start, "unterminated string literal")
			}
			switch l.input[i] {
			case '"':
				b.WriteByte('"')
			case '\\':
				b.WriteByte('\\')
			case 'n':
				b.WriteByte('\n')
			case 't':
				b.WriteByte('\t')
			case 'r':
				b.WriteByte('\r')
			default:
				// Preserve an unknown escape verbatim so regex patterns like
				// \d survive a round trip unchanged.
				b.WriteByte('\\')
				b.WriteByte(l.input[i])
			}
			i++
		case '"':
			l.pos = i + 1
			return token{typ: tokString, val: b.String(), pos: start}, nil
		default:
			b.WriteByte(c)
			i++
		}
	}
	return token{}, perrorf(start, "unterminated string literal")
}

func (l *lexer) lexIdent(start int) (token, error) {
	i := start
	for i < len(l.input) && isIdentPart(l.input[i]) {
		i++
	}
	l.pos = i
	return token{typ: tokIdent, val: l.input[start:i], pos: start}, nil
}

// lexNumberOrDuration prefers a duration reading when the run validates as one
// (5m, 1h30m, 500ms) and otherwise falls back to a numeric literal, so
// scientific notation like 1e3 is never mistaken for a duration.
func (l *lexer) lexNumberOrDuration(start int) (token, error) {
	if run, ok := scanDurationRun(l.input, start); ok {
		if d, err := time.ParseDuration(run); err == nil {
			l.pos = start + len(run)
			return token{typ: tokDuration, val: run, dur: d, pos: start}, nil
		}
	}
	return l.lexNumber(start)
}

func (l *lexer) lexNumber(start int) (token, error) {
	i := start
	for i < len(l.input) && isDigit(l.input[i]) {
		i++
	}
	if i < len(l.input) && l.input[i] == '.' {
		i++
		for i < len(l.input) && isDigit(l.input[i]) {
			i++
		}
	}
	// Only consume an exponent when it is actually followed by digits; a bare
	// trailing 'e' belongs to whatever token comes next.
	if i < len(l.input) && (l.input[i] == 'e' || l.input[i] == 'E') {
		j := i + 1
		if j < len(l.input) && (l.input[j] == '+' || l.input[j] == '-') {
			j++
		}
		if j < len(l.input) && isDigit(l.input[j]) {
			i = j + 1
			for i < len(l.input) && isDigit(l.input[i]) {
				i++
			}
		}
	}
	text := l.input[start:i]
	v, err := strconv.ParseFloat(text, 64)
	if err != nil {
		return token{}, perrorf(start, "invalid number %q", text)
	}
	l.pos = i
	return token{typ: tokNumber, val: text, num: v, pos: start}, nil
}

// scanDurationRun greedily consumes a duration-shaped run: digits, dots and the
// unit letters time.ParseDuration recognises. It reports ok only when at least
// one digit and one unit were seen, so a plain number is rejected here and
// handled as a number instead.
func scanDurationRun(input string, start int) (string, bool) {
	i := start
	sawDigit, sawUnit := false, false
	for i < len(input) {
		c := input[i]
		switch {
		case c >= '0' && c <= '9':
			sawDigit = true
			i++
		case c == '.':
			i++
		case isDurUnit(c):
			sawUnit = true
			i++
		case strings.HasPrefix(input[i:], "µs") || strings.HasPrefix(input[i:], "μs"):
			// Microsecond unit spelled with the micro sign is multibyte.
			sawUnit = true
			i += len("µs")
		default:
			goto done
		}
	}
done:
	if !sawDigit || !sawUnit {
		return "", false
	}
	return input[start:i], true
}

func isDurUnit(c byte) bool {
	switch c {
	case 'n', 'u', 'm', 's', 'h':
		return true
	}
	return false
}

func isDigit(c byte) bool      { return c >= '0' && c <= '9' }
func isIdentStart(c byte) bool { return c == '_' || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') }
func isIdentPart(c byte) bool  { return isIdentStart(c) || isDigit(c) }

// perrorf builds a positioned parse error. Lexer and parser share one format so
// every failure reads "parse error at position N: …".
func perrorf(pos int, format string, a ...any) error {
	return fmt.Errorf("parse error at position %d: %s", pos, fmt.Sprintf(format, a...))
}
