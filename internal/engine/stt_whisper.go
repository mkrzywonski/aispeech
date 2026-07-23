package engine

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"unicode"
)

// WhisperSTT transcribes audio by invoking the whisper.cpp CLI.
type WhisperSTT struct {
	bin   string
	model string
	lang  string
}

// NewWhisperSTT validates the binary and model and returns a transcriber.
func NewWhisperSTT(bin, model, lang string) (*WhisperSTT, error) {
	if bin == "" {
		bin = "whisper-cli"
	}
	path, err := exec.LookPath(bin)
	if err != nil {
		return nil, fmt.Errorf("whisper binary %q not found: %w", bin, err)
	}
	if model == "" {
		return nil, fmt.Errorf("no whisper model configured")
	}
	if _, err := os.Stat(model); err != nil {
		return nil, fmt.Errorf("whisper model %q: %w", model, err)
	}
	if lang == "" {
		lang = "auto"
	}
	return &WhisperSTT{bin: path, model: model, lang: lang}, nil
}

// Transcribe writes the segment to a temp WAV and runs whisper.cpp over it.
func (w *WhisperSTT) Transcribe(ctx context.Context, seg Segment) (string, error) {
	tmp, err := os.CreateTemp("", "aispeech-*.wav")
	if err != nil {
		return "", err
	}
	path := tmp.Name()
	tmp.Close()
	defer os.Remove(path)

	if err := writeWAVFile(path, seg.PCM, seg.SampleRate); err != nil {
		return "", err
	}

	// -nt: no timestamps, -np: no progress prints. Transcript goes to stdout.
	cmd := exec.CommandContext(ctx, w.bin,
		"-m", w.model, "-f", path, "-l", w.lang, "-nt", "-np")
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("whisper %s: %w", filepath.Base(w.bin), err)
	}
	return cleanTranscript(string(out)), nil
}

func cleanTranscript(s string) string {
	var lines []string
	for _, ln := range strings.Split(s, "\n") {
		if t := strings.TrimSpace(ln); t != "" {
			lines = append(lines, t)
		}
	}
	text := strings.Join(lines, " ")
	// Drop whisper non-speech annotations: the whole utterance wrapped in [], (),
	// or ** — e.g. "[BLANK_AUDIO]", "(keyboard clicking)", "*laughs*" — and
	// utterances with no actual words (e.g. "...", "♪♪♪"). These come from the VAD
	// triggering on noise and are never useful voice commands.
	if isAnnotation(text) || !hasWord(text) {
		return ""
	}
	return text
}

// isAnnotation reports whether s is a single bracketed/parenthesized/starred
// non-speech marker spanning the whole string.
func isAnnotation(s string) bool {
	if len(s) < 2 {
		return false
	}
	for _, p := range [][2]byte{{'[', ']'}, {'(', ')'}, {'*', '*'}} {
		if s[0] == p[0] && s[len(s)-1] == p[1] &&
			!strings.ContainsRune(s[1:len(s)-1], rune(p[0])) &&
			!strings.ContainsRune(s[1:len(s)-1], rune(p[1])) {
			return true
		}
	}
	return false
}

func hasWord(s string) bool {
	for _, r := range s {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			return true
		}
	}
	return false
}
