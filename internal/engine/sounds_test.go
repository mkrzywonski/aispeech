package engine

import "testing"

func TestGenerateSound(t *testing.T) {
	for _, name := range SoundNames() {
		pcm, rate, ok := generateSound(name)
		if !ok || rate != soundRate || len(pcm) == 0 {
			t.Fatalf("generateSound(%q) = (len %d, rate %d, ok %v)", name, len(pcm), rate, ok)
		}
	}
	if _, _, ok := generateSound("nope"); ok {
		t.Fatal("unknown sound should not resolve")
	}
	if len(SoundNames()) == 0 {
		t.Fatal("no sound names")
	}
}
