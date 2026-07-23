package engine

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
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
	// whisper emits bracketed markers like [BLANK_AUDIO] on silence.
	if strings.HasPrefix(text, "[") && strings.HasSuffix(text, "]") {
		return ""
	}
	return text
}
