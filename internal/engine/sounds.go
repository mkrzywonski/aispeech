package engine

import (
	"fmt"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"sort"
)

// note is one tone in a generated sound; freq 0 is a rest. dur is the note's
// full time slot — the tone attacks quickly then rings out (decays) within it.
type note struct {
	freq float64
	dur  float64 // seconds
}

const soundRate = 44100

// soundCatalog maps built-in sound names to note sequences. These are generated
// on the fly (no asset files) with a warm bell/marimba timbre (harmonics +
// exponential decay), tuned to musical intervals so they read as pleasant
// notifications rather than raw beeps.
var soundCatalog = map[string][]note{
	// gentle rising bell, a perfect fifth up, with a lingering ring
	"chime": {{659.25, 0.24}, {987.77, 0.75}}, // E5 -> B5
	// bright ascending major arpeggio — a satisfied "done"
	"success": {{523.25, 0.13}, {659.25, 0.13}, {783.99, 0.13}, {1046.50, 0.7}}, // C5 E5 G5 C6
	// soft descending step — "nope" without a harsh buzz
	"error": {{493.88, 0.17}, {329.63, 0.6}}, // B4 -> E4
	// two soft attention taps
	"alert": {{880.00, 0.16}, {0, 0.05}, {880.00, 0.55}}, // A5
	// urgent but still musical — a quick triple with a ring-out
	"alarm": {{987.77, 0.13}, {0, 0.05}, {987.77, 0.13}, {0, 0.05}, {987.77, 0.13}, {0, 0.05}, {987.77, 0.5}}, // B5 x4
	// one clean, bright bell
	"ding": {{1046.50, 0.7}}, // C6
}

// SoundNames returns the built-in sound names, sorted.
func SoundNames() []string {
	names := make([]string, 0, len(soundCatalog))
	for n := range soundCatalog {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

// generateSound renders a named built-in sound to mono float32 PCM.
func generateSound(name string) (pcm []float32, sampleRate int, ok bool) {
	notes, ok := soundCatalog[name]
	if !ok {
		return nil, 0, false
	}
	return synth(notes, soundRate), soundRate, true
}

// soundNameRE limits sound names to safe filename characters (no path traversal).
var soundNameRE = regexp.MustCompile(`^[A-Za-z0-9_-]{1,40}$`)

// SoundNameOK reports whether name is a valid custom-sound name.
func SoundNameOK(name string) bool { return soundNameRE.MatchString(name) }

// CustomSoundPath returns the on-disk path for a custom sound and whether name
// is valid. dir may be "".
func CustomSoundPath(dir, name string) (string, bool) {
	if dir == "" || !SoundNameOK(name) {
		return "", false
	}
	return filepath.Join(dir, name+".wav"), true
}

// resolveSound returns PCM for a name, preferring a custom <dir>/<name>.wav file
// over the built-in synth.
func resolveSound(dir, name string) (pcm []float32, sampleRate int, ok bool) {
	if p, valid := CustomSoundPath(dir, name); valid {
		if pcm, sr, err := readWAVFile(p); err == nil {
			return pcm, sr, true
		}
	}
	return generateSound(name)
}

// SoundInfo describes one playable sound for the UI.
type SoundInfo struct {
	Name    string `json:"name"`
	Builtin bool   `json:"builtin"` // has a built-in synth
	Custom  bool   `json:"custom"`  // has a user WAV (overrides the built-in if both)
}

// ListSounds returns the built-in sounds plus any custom WAVs in dir, sorted.
func ListSounds(dir string) []SoundInfo {
	seen := map[string]*SoundInfo{}
	for _, n := range SoundNames() {
		seen[n] = &SoundInfo{Name: n, Builtin: true}
	}
	if dir != "" {
		if entries, err := os.ReadDir(dir); err == nil {
			for _, e := range entries {
				if e.IsDir() || filepath.Ext(e.Name()) != ".wav" {
					continue
				}
				n := e.Name()[:len(e.Name())-len(".wav")]
				if !SoundNameOK(n) {
					continue
				}
				if si, ok := seen[n]; ok {
					si.Custom = true
				} else {
					seen[n] = &SoundInfo{Name: n, Custom: true}
				}
			}
		}
	}
	out := make([]SoundInfo, 0, len(seen))
	for _, si := range seen {
		out = append(out, *si)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// ValidateWAVFile reports whether path is a WAV this engine can play.
func ValidateWAVFile(path string) error {
	if _, _, err := readWAVFile(path); err != nil {
		return fmt.Errorf("not a playable WAV: %w", err)
	}
	return nil
}

// partials shape the timbre: a fundamental plus a few harmonics with their own
// (relative) decay rates. Higher partials decay faster, and one slightly
// inharmonic partial adds a bell-like shimmer. amps sum to partialNorm.
var partials = []struct{ mult, amp, decay float64 }{
	{1.00, 1.00, 1.00}, // fundamental
	{2.00, 0.45, 0.70}, // octave
	{3.00, 0.22, 0.50}, // fifth above that
	{4.20, 0.10, 0.35}, // inharmonic shimmer
}

const partialNorm = 1.77 // sum of partial amps, for normalization

// synth renders a note sequence with a warm bell/marimba timbre: a fast attack
// (no click) followed by an exponential ring-out, summed over a few harmonics.
func synth(notes []note, rate int) []float32 {
	const amp = 0.32
	attack := int(0.004 * float64(rate)) // 4 ms
	var out []float32
	for _, nt := range notes {
		n := int(float64(rate) * nt.dur)
		seg := make([]float32, n)
		if nt.freq > 0 {
			// Ring for most of the slot; higher partials fade quicker.
			tau := nt.dur * 0.45
			for i := 0; i < n; i++ {
				t := float64(i) / float64(rate)
				var s float64
				for _, p := range partials {
					s += p.amp * math.Exp(-t/(tau*p.decay)) * math.Sin(2*math.Pi*nt.freq*p.mult*t)
				}
				env := 1.0
				if i < attack {
					env = float64(i) / float64(attack)
				}
				seg[i] = float32(amp * env * s / partialNorm)
			}
		}
		out = append(out, seg...)
	}
	return out
}
