package rules

import (
	"testing"
	"time"
)

func TestLexTokenStreams(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  []token
	}{
		{
			name:  "comparison",
			input: "cpu_usage > 90",
			want: []token{
				{typ: tokIdent, val: "cpu_usage"},
				{typ: tokOp, val: ">"},
				{typ: tokNumber, val: "90", num: 90},
			},
		},
		{
			name:  "matchers and range",
			input: `rate(http_requests_total{code=~"5.."}[5m])`,
			want: []token{
				{typ: tokIdent, val: "rate"},
				{typ: tokLParen, val: "("},
				{typ: tokIdent, val: "http_requests_total"},
				{typ: tokLBrace, val: "{"},
				{typ: tokIdent, val: "code"},
				{typ: tokOp, val: "=~"},
				{typ: tokString, val: "5.."},
				{typ: tokRBrace, val: "}"},
				{typ: tokLBracket, val: "["},
				{typ: tokDuration, val: "5m", dur: 5 * time.Minute},
				{typ: tokRBracket, val: "]"},
				{typ: tokRParen, val: ")"},
			},
		},
		{
			name:  "all matcher operators",
			input: `x{a="1",b!="2",c=~"3",d!~"4"}`,
			want: []token{
				{typ: tokIdent, val: "x"},
				{typ: tokLBrace, val: "{"},
				{typ: tokIdent, val: "a"}, {typ: tokOp, val: "="}, {typ: tokString, val: "1"}, {typ: tokComma, val: ","},
				{typ: tokIdent, val: "b"}, {typ: tokOp, val: "!="}, {typ: tokString, val: "2"}, {typ: tokComma, val: ","},
				{typ: tokIdent, val: "c"}, {typ: tokOp, val: "=~"}, {typ: tokString, val: "3"}, {typ: tokComma, val: ","},
				{typ: tokIdent, val: "d"}, {typ: tokOp, val: "!~"}, {typ: tokString, val: "4"},
				{typ: tokRBrace, val: "}"},
			},
		},
		{
			name:  "scientific number not a duration",
			input: "1.5e3 + .25",
			want: []token{
				{typ: tokNumber, val: "1.5e3", num: 1500},
				{typ: tokOp, val: "+"},
				{typ: tokNumber, val: ".25", num: 0.25},
			},
		},
		{
			name:  "compound duration",
			input: "for 1h30m",
			want: []token{
				{typ: tokIdent, val: "for"},
				{typ: tokDuration, val: "1h30m", dur: time.Hour + 30*time.Minute},
			},
		},
		{
			name:  "arithmetic operators",
			input: "a + b - c * d / e % f",
			want: []token{
				{typ: tokIdent, val: "a"}, {typ: tokOp, val: "+"},
				{typ: tokIdent, val: "b"}, {typ: tokOp, val: "-"},
				{typ: tokIdent, val: "c"}, {typ: tokOp, val: "*"},
				{typ: tokIdent, val: "d"}, {typ: tokOp, val: "/"},
				{typ: tokIdent, val: "e"}, {typ: tokOp, val: "%"},
				{typ: tokIdent, val: "f"},
			},
		},
		{
			name:  "string escapes",
			input: `x{a="he\"llo\\"}`,
			want: []token{
				{typ: tokIdent, val: "x"}, {typ: tokLBrace, val: "{"},
				{typ: tokIdent, val: "a"}, {typ: tokOp, val: "="}, {typ: tokString, val: `he"llo\`},
				{typ: tokRBrace, val: "}"},
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			toks, err := lex(tc.input)
			if err != nil {
				t.Fatalf("lex(%q): unexpected error: %v", tc.input, err)
			}
			// The final token is always EOF; drop it before comparing bodies.
			if len(toks) == 0 || toks[len(toks)-1].typ != tokEOF {
				t.Fatalf("lex(%q): missing trailing EOF token", tc.input)
			}
			got := toks[:len(toks)-1]
			if len(got) != len(tc.want) {
				t.Fatalf("lex(%q): got %d tokens, want %d\n got=%v", tc.input, len(got), len(tc.want), got)
			}
			for i, w := range tc.want {
				g := got[i]
				if g.typ != w.typ || g.val != w.val || g.num != w.num || g.dur != w.dur {
					t.Errorf("token %d = {typ:%d val:%q num:%v dur:%v}, want {typ:%d val:%q num:%v dur:%v}",
						i, g.typ, g.val, g.num, g.dur, w.typ, w.val, w.num, w.dur)
				}
			}
		})
	}
}

func TestLexPositions(t *testing.T) {
	toks, err := lex("ab > 90")
	if err != nil {
		t.Fatalf("lex: %v", err)
	}
	wantPos := []int{0, 3, 5, 7} // ab, >, 90, EOF
	for i, want := range wantPos {
		if toks[i].pos != want {
			t.Errorf("token %d pos = %d, want %d", i, toks[i].pos, want)
		}
	}
}

func TestLexErrors(t *testing.T) {
	tests := []struct {
		name  string
		input string
	}{
		{"unterminated string", `x{a="oops}`},
		{"bad character", "a @ b"},
		{"lone bang", "a ! b"},
		{"dangling escape", `x{a="oops\`},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := lex(tc.input); err == nil {
				t.Fatalf("lex(%q): expected error, got nil", tc.input)
			}
		})
	}
}
