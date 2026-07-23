package engine

import "testing"

func TestCleanTranscript(t *testing.T) {
	cases := []struct{ in, want string }{
		{"(keyboard clicking)", ""},
		{"[BLANK_AUDIO]", ""},
		{"[ Silence ]", ""},
		{"*laughs*", ""},
		{"...", ""},
		{"♪♪♪", ""},
		{"   ", ""},
		{"Hello.", "Hello."},
		{"Claude, run the tests", "Claude, run the tests"},
		{"(pause) run the tests", "(pause) run the tests"}, // real words -> keep
	}
	for _, c := range cases {
		if got := cleanTranscript(c.in); got != c.want {
			t.Errorf("cleanTranscript(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}
