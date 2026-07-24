package session

import (
	"context"
	"testing"
	"time"
)

// pair attaches and pairs a session, returning its id.
func pair(t *testing.T, r *Registry, id, client string) {
	t.Helper()
	r.Attach(id, client)
	if _, err := r.Pair(id, "browser-cookie"); err != nil {
		t.Fatalf("pair %s: %v", client, err)
	}
}

func TestSessionWordRouting(t *testing.T) {
	r := New()
	pair(t, r, "id1", "claude") // first paired -> focus
	pair(t, r, "id2", "codex")

	// codex is listening; "codex, ..." should route to it with the name stripped.
	got := make(chan Utterance, 1)
	go func() {
		u, status := r.Listen(context.Background(), "id2", time.Second)
		if status == "ok" {
			got <- u
		}
	}()
	time.Sleep(50 * time.Millisecond)
	r.Deliver("codex, run the report")

	select {
	case u := <-got:
		if u.Text != "run the report" {
			t.Fatalf("text = %q, want %q", u.Text, "run the report")
		}
		if u.Target != "codex" {
			t.Fatalf("target = %q, want codex", u.Target)
		}
	case <-time.After(time.Second):
		t.Fatal("codex did not receive routed utterance")
	}

	// Focus should now be sticky on codex.
	if f := focusName(r); f != "codex" {
		t.Fatalf("focus = %q, want codex after routing", f)
	}
}

func TestBareNameSwitchesFocusOnly(t *testing.T) {
	r := New()
	pair(t, r, "id1", "claude")
	pair(t, r, "id2", "codex")
	if f := focusName(r); f != "claude" {
		t.Fatalf("initial focus = %q, want claude", f)
	}
	r.Deliver("codex") // bare name = focus switch, no delivery
	if f := focusName(r); f != "codex" {
		t.Fatalf("focus = %q, want codex", f)
	}
}

func TestDropWhenNotListening(t *testing.T) {
	r := New()
	pair(t, r, "id1", "claude") // focused, but never calls listen()
	r.Deliver("do something")   // no outstanding listen -> dropped (recognized entry only)
	_, log := r.Snapshot()
	last, ok := lastOfKind(log, "recognized")
	if !ok {
		t.Fatal("no recognized entry recorded")
	}
	if last.Outcome != "dropped" || last.Session != "claude" || last.Text != "do something" {
		t.Fatalf("want dropped recognized entry, got %+v", last)
	}
}

func lastOfKind(log []LogEntry, kind string) (LogEntry, bool) {
	for i := len(log) - 1; i >= 0; i-- {
		if log[i].Kind == kind {
			return log[i], true
		}
	}
	return LogEntry{}, false
}

func focusName(r *Registry) string {
	views, _ := r.Snapshot()
	for _, v := range views {
		if v.Focused {
			return v.Name
		}
	}
	return ""
}

func TestUnpairAll(t *testing.T) {
	r := New()
	pair(t, r, "id1", "claude") // becomes focus
	pair(t, r, "id2", "codex")
	if focusName(r) == "" {
		t.Fatal("expected a focused session before unpair")
	}

	r.UnpairAll()

	views, _ := r.Snapshot()
	if len(views) != 2 {
		t.Fatalf("sessions should remain connected, got %d", len(views))
	}
	for _, v := range views {
		if v.Paired {
			t.Fatalf("%s still paired after UnpairAll", v.Name)
		}
		if v.Focused {
			t.Fatalf("%s still focused after UnpairAll", v.Name)
		}
	}
	if focusName(r) != "" {
		t.Fatal("focus should be cleared after UnpairAll")
	}
	// A subsequent listen must be refused until re-pairing.
	if _, status := r.Listen(context.Background(), "id1", 10*time.Millisecond); status != "unpaired" {
		t.Fatalf("listen after unpair = %q, want unpaired", status)
	}
}

func TestDefaultNameIsSingleWord(t *testing.T) {
	r := New()
	s := r.Attach("id1", "claude-code")
	if s.Name != "claude" {
		t.Fatalf("default name = %q, want %q (single word wake word)", s.Name, "claude")
	}
	if s.ClientName != "claude-code" {
		t.Fatalf("client name = %q, want the full client id", s.ClientName)
	}
}

func TestFirstWord(t *testing.T) {
	for in, want := range map[string]string{
		"claude-code": "claude",
		"claude":      "claude",
		"gemini_cli":  "gemini",
		"  agent 2":   "agent",
		"":            "",
		"--":          "",
	} {
		if got := firstWord(in); got != want {
			t.Errorf("firstWord(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestStripLeadingTokens(t *testing.T) {
	name := wordTokens("claude-code") // ["claude","code"]

	// Spoken "Claude code, ..." (no hyphen, mixed case) still matches, and the
	// command remainder keeps its original casing/punctuation.
	if rest, ok := stripLeadingTokens("Claude code, run the Tests!", name); !ok || rest != "run the Tests!" {
		t.Fatalf("hyphen/space: got (%q,%v), want (%q,true)", rest, ok, "run the Tests!")
	}
	// Bare name -> empty remainder (focus switch only).
	if rest, ok := stripLeadingTokens("claude code", name); !ok || rest != "" {
		t.Fatalf("bare name: got (%q,%v)", rest, ok)
	}
	// A different name does not match.
	if _, ok := stripLeadingTokens("codex do it", name); ok {
		t.Fatal("codex should not match claude-code")
	}
	// Token matching must not treat one word as a prefix of a longer word.
	if _, ok := stripLeadingTokens("claudia hello", wordTokens("claude")); ok {
		t.Fatal("claude should not match claudia")
	}
}

func TestMultiWordNameRoutesFromSpeech(t *testing.T) {
	r := New()
	pair(t, r, "id1", "gemini") // focus
	pair(t, r, "id2", "claude")
	if err := r.Rename("id2", "claude code"); err != nil { // user picks a two-word wake word
		t.Fatal(err)
	}

	got := make(chan Utterance, 1)
	go func() {
		if u, status := r.Listen(context.Background(), "id2", time.Second); status == "ok" {
			got <- u
		}
	}()
	time.Sleep(50 * time.Millisecond)
	r.Deliver("Claude code, run the tests") // as whisper would transcribe it

	select {
	case u := <-got:
		if u.Text != "run the tests" || u.Target != "claude code" {
			t.Fatalf("routed = %+v, want text=%q target=%q", u, "run the tests", "claude code")
		}
	case <-time.After(time.Second):
		t.Fatal("multi-word name did not route from spoken words")
	}
}
