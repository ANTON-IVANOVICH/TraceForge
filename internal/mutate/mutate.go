// Package mutate implements mutation testing: it makes small, semantics-changing
// edits to the source and checks that the test suite notices.
//
// Coverage tells you a line was executed. It does not tell you the line was
// checked. This test has 100% coverage of add and asserts nothing:
//
//	func add(a, b int) int { return a + b }
//	func TestAdd(t *testing.T) { add(1, 2) }
//
// Mutation testing asks the only question that matters: if I change `+` to `-`,
// does anything go red? A mutant that survives marks a line your tests execute
// but do not verify. That is a hole coverage cannot see.
//
// Mutants are produced by splicing bytes at the offset of a single token, not by
// re-printing the AST. Re-printing normalises formatting and relocates comments,
// which would rewrite `//go:build` lines and make the diff between original and
// mutant unreadable. A byte splice changes exactly one operator and nothing else.
package mutate

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"strconv"
	"strings"
)

// Mutant is one candidate edit: replace the bytes at Offset with New.
type Mutant struct {
	File    string // absolute path of the file the mutant edits
	Offset  int    // byte offset of the token being replaced
	Old     string // the token text being replaced, for verification
	New     string // the replacement
	Mutator string // which rule produced it, e.g. "conditional"
	Line    int
	Column  int
}

func (m Mutant) String() string {
	return fmt.Sprintf("%s:%d:%d: %s: %s -> %s", m.File, m.Line, m.Column, m.Mutator, m.Old, m.New)
}

// Apply returns src with the mutant's edit spliced in. It verifies that the
// bytes at the offset are the ones the mutant expects, so a stale offset — from
// a file edited between generation and application — fails loudly rather than
// corrupting the source at a random point.
func (m Mutant) Apply(src []byte) ([]byte, error) {
	end := m.Offset + len(m.Old)
	if m.Offset < 0 || end > len(src) {
		return nil, fmt.Errorf("mutant offset %d out of range for %s", m.Offset, m.File)
	}
	if got := string(src[m.Offset:end]); got != m.Old {
		return nil, fmt.Errorf("%s: expected %q at offset %d, found %q", m.File, m.Old, m.Offset, got)
	}
	out := make([]byte, 0, len(src)+len(m.New)-len(m.Old))
	out = append(out, src[:m.Offset]...)
	out = append(out, m.New...)
	out = append(out, src[end:]...)
	return out, nil
}

// binaryOps maps an operator to the operators it is mutated into. Each swap is
// chosen to be a plausible bug rather than an obvious one: `>` to `>=` is the
// off-by-one a reviewer misses, while `>` to `<` is a change any test notices.
var binaryOps = map[token.Token][]token.Token{
	token.GTR: {token.GEQ}, // >  ->  >=
	token.GEQ: {token.GTR}, // >= ->  >
	token.LSS: {token.LEQ}, // <  ->  <=
	token.LEQ: {token.LSS}, // <= ->  <
	token.EQL: {token.NEQ}, // == ->  !=
	token.NEQ: {token.EQL}, // != ->  ==

	token.LAND: {token.LOR}, // && ->  ||
	token.LOR:  {token.LAND},

	token.ADD: {token.SUB}, // +  ->  -
	token.SUB: {token.ADD},
	token.MUL: {token.QUO}, // *  ->  /
	token.QUO: {token.MUL},
	token.REM: {token.MUL},
}

func mutatorName(op token.Token) string {
	switch op {
	case token.GTR, token.GEQ, token.LSS, token.LEQ, token.EQL, token.NEQ:
		return "conditional"
	case token.LAND, token.LOR:
		return "logical"
	default:
		return "arithmetic"
	}
}

// Generate parses src and returns every mutant it can make. It never mutates a
// generated file, and it never mutates inside a test — grading a test suite by
// breaking the test suite proves nothing.
func Generate(fset *token.FileSet, path string, src []byte) ([]Mutant, error) {
	if isGenerated(src) {
		return nil, nil
	}

	file, err := parser.ParseFile(fset, path, src, parser.ParseComments|parser.SkipObjectResolution)
	if err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}

	var mutants []Mutant
	add := func(pos token.Pos, old, new, mutator string) {
		p := fset.Position(pos)
		mutants = append(mutants, Mutant{
			File: path, Offset: p.Offset, Old: old, New: new,
			Mutator: mutator, Line: p.Line, Column: p.Column,
		})
	}

	ast.Inspect(file, func(n ast.Node) bool {
		switch node := n.(type) {
		case *ast.BinaryExpr:
			for _, to := range binaryOps[node.Op] {
				add(node.OpPos, node.Op.String(), to.String(), mutatorName(node.Op))
			}

		case *ast.UnaryExpr:
			// Deleting a `!` inverts a condition. Deleting the `-` of a negative
			// literal would too, but `-1` is usually a constant rather than a
			// decision, so only the logical negation is worth a mutant.
			if node.Op == token.NOT {
				add(node.OpPos, "!", "", "negation")
			}

		case *ast.IncDecStmt:
			switch node.Tok {
			case token.INC:
				add(node.TokPos, "++", "--", "increment")
			case token.DEC:
				add(node.TokPos, "--", "++", "increment")
			}

		case *ast.Ident:
			// true/false are predeclared identifiers, not keywords, so they land
			// here. A shadowed `true` would be mutated wrongly, but a variable
			// named true is not a thing that happens in code worth grading.
			switch node.Name {
			case "true":
				add(node.NamePos, "true", "false", "boolean")
			case "false":
				add(node.NamePos, "false", "true", "boolean")
			}

		case *ast.BasicLit:
			if node.Kind != token.INT {
				return true
			}
			// Only plain decimal literals: incrementing 0x1F or 0b1010 as text
			// would produce a mutant nobody can read, and incrementing a huge
			// constant may not fit its type.
			v, err := strconv.ParseInt(node.Value, 10, 32)
			if err != nil {
				return true
			}
			add(node.ValuePos, node.Value, strconv.FormatInt(v+1, 10), "literal")
		}
		return true
	})

	return mutants, nil
}

// isGenerated recognises the convention from https://go.dev/s/generatedcode.
// Mutating generated code grades the generator, not the tests.
func isGenerated(src []byte) bool {
	// The marker must appear before the package clause.
	head := string(src)
	if i := strings.Index(head, "\npackage "); i >= 0 {
		head = head[:i]
	}
	for _, line := range strings.Split(head, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "// Code generated ") && strings.HasSuffix(line, " DO NOT EDIT.") {
			return true
		}
	}
	return false
}
