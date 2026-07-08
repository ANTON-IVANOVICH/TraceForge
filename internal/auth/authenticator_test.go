package auth

import "testing"

func TestBearerToken(t *testing.T) {
	t.Parallel()
	cases := map[string]struct {
		in    string
		want  string
		wantK bool
	}{
		"canonical":  {"Bearer abc", "abc", true},
		"lowercase":  {"bearer abc", "abc", true},
		"uppercase":  {"BEARER abc", "abc", true},
		"mixed":      {"BeArEr abc", "abc", true},
		"padded":     {"Bearer   abc  ", "abc", true},
		"wrong":      {"Basic abc", "", false},
		"empty":      {"", "", false},
		"scheleonly": {"Bearer ", "", true},
	}
	for name, c := range cases {
		got, ok := BearerToken(c.in)
		if ok != c.wantK || got != c.want {
			t.Errorf("%s: BearerToken(%q) = (%q,%v), want (%q,%v)", name, c.in, got, ok, c.want, c.wantK)
		}
	}
}
