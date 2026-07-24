package engine

import (
	"path/filepath"
	"testing"
)

func TestSoundNameOK(t *testing.T) {
	for _, n := range []string{"beep", "my-sound", "alarm_2", "Chime"} {
		if !SoundNameOK(n) {
			t.Errorf("SoundNameOK(%q) = false, want true", n)
		}
	}
	for _, n := range []string{"", "../evil", "a/b", "a.b", "space bar", string(make([]byte, 41))} {
		if SoundNameOK(n) {
			t.Errorf("SoundNameOK(%q) = true, want false", n)
		}
	}
}

func TestCustomSoundPathRejectsBadNames(t *testing.T) {
	if _, ok := CustomSoundPath("/snd", "../etc/passwd"); ok {
		t.Fatal("path traversal accepted")
	}
	if _, ok := CustomSoundPath("", "beep"); ok {
		t.Fatal("empty dir accepted")
	}
	if p, ok := CustomSoundPath("/snd", "beep"); !ok || p != filepath.Join("/snd", "beep.wav") {
		t.Fatalf("CustomSoundPath = (%q,%v)", p, ok)
	}
}

func TestListAndResolveCustomSounds(t *testing.T) {
	dir := t.TempDir()
	// A brand-new custom sound and an override of the built-in "ding".
	beep := []float32{0.1, 0.2, 0.3}
	if err := writeWAVFile(filepath.Join(dir, "beep.wav"), beep, 16000); err != nil {
		t.Fatal(err)
	}
	customDing := make([]float32, 12345) // distinct length from the synth
	if err := writeWAVFile(filepath.Join(dir, "ding.wav"), customDing, 16000); err != nil {
		t.Fatal(err)
	}

	byName := map[string]SoundInfo{}
	for _, s := range ListSounds(dir) {
		byName[s.Name] = s
	}
	if s := byName["beep"]; !s.Custom || s.Builtin {
		t.Fatalf("beep = %+v, want custom-only", s)
	}
	if s := byName["ding"]; !s.Custom || !s.Builtin {
		t.Fatalf("ding = %+v, want builtin+custom", s)
	}
	if s := byName["success"]; s.Custom || !s.Builtin {
		t.Fatalf("success = %+v, want builtin-only", s)
	}

	// A custom name resolves to its file.
	if pcm, _, ok := resolveSound(dir, "beep"); !ok || len(pcm) != len(beep) {
		t.Fatalf("resolve beep = (%d,%v)", len(pcm), ok)
	}
	// An override wins over the built-in synth.
	if pcm, _, ok := resolveSound(dir, "ding"); !ok || len(pcm) != len(customDing) {
		t.Fatalf("resolve ding override = (%d,%v), want %d", len(pcm), ok, len(customDing))
	}
	// A built-in with no override still resolves.
	if _, _, ok := resolveSound(dir, "success"); !ok {
		t.Fatal("resolve built-in success failed")
	}
	// Unknown resolves to nothing.
	if _, _, ok := resolveSound(dir, "nope"); ok {
		t.Fatal("resolve unknown succeeded")
	}
}
