package engine

import (
	"math"
	"sort"
)

// note is one tone in a generated sound; freq 0 is a rest.
type note struct {
	freq float64
	dur  float64 // seconds
}

const soundRate = 44100

// soundCatalog maps built-in sound names to note sequences. These are generated
// on the fly (no asset files) and are intended for short notifications/alerts.
var soundCatalog = map[string][]note{
	"chime":   {{660, 0.18}, {880, 0.28}},
	"success": {{523.25, 0.12}, {659.25, 0.12}, {783.99, 0.24}}, // C5 E5 G5, rising
	"error":   {{392, 0.16}, {262, 0.30}},                       // G4 -> C4, falling
	"alert":   {{880, 0.12}, {0, 0.08}, {880, 0.12}},
	"alarm":   {{988, 0.1}, {0, 0.06}, {988, 0.1}, {0, 0.06}, {988, 0.1}, {0, 0.06}, {988, 0.16}},
	"ding":    {{1046.5, 0.4}}, // C6
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

// synth renders a note sequence with short fade envelopes (no clicks).
func synth(notes []note, rate int) []float32 {
	const amp = 0.3
	fade := int(0.008 * float64(rate))
	var out []float32
	for _, nt := range notes {
		n := int(float64(rate) * nt.dur)
		for i := 0; i < n; i++ {
			if nt.freq == 0 {
				out = append(out, 0)
				continue
			}
			env := 1.0
			if i < fade {
				env = float64(i) / float64(fade)
			} else if i > n-fade {
				env = float64(n-i) / float64(fade)
			}
			out = append(out, float32(amp*env*math.Sin(2*math.Pi*nt.freq*float64(i)/float64(rate))))
		}
	}
	return out
}
