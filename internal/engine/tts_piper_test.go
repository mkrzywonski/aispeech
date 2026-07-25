package engine

import "testing"

func TestWithLeadIn(t *testing.T) {
	pcm := []float32{1, 2, 3}
	const sr = 16000
	const wantLead = 1920 // 120ms at 16kHz
	out := withLeadIn(pcm, sr)

	if len(out) != wantLead+len(pcm) {
		t.Fatalf("length = %d, want %d (%d lead-in + %d pcm)", len(out), wantLead+len(pcm), wantLead, len(pcm))
	}
	for i := 0; i < wantLead; i++ {
		if out[i] != 0 {
			t.Fatalf("lead-in sample %d = %v, want silence", i, out[i])
		}
	}
	if out[wantLead] != 1 || out[wantLead+2] != 3 {
		t.Fatal("original pcm not preserved after the lead-in")
	}
}
