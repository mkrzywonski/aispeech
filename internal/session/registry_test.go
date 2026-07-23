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
	pair(t, r, "id1", "claude")   // focused, but never calls listen()
	r.Deliver("do something")     // no outstanding listen -> dropped (transcript only)
	_, _, transcripts := r.Snapshot()
	if len(transcripts) == 0 {
		t.Fatal("no transcript recorded")
	}
	last := transcripts[len(transcripts)-1]
	if last.Outcome != "dropped" || last.Target != "claude" || last.Text != "do something" {
		t.Fatalf("want dropped transcript, got %+v", last)
	}
}

func focusName(r *Registry) string {
	views, _, _ := r.Snapshot()
	for _, v := range views {
		if v.Focused {
			return v.Name
		}
	}
	return ""
}
