package engine

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
)

// player plays mono float32 PCM at a sample rate (satisfied by *AudioContext).
type player interface {
	Play(pcm []float32, sampleRate int) error
}

// PiperTTS synthesizes speech with the piper CLI and plays it via the audio
// context.
type PiperTTS struct {
	bin   string
	voice string
	play  player
}

// NewPiperTTS validates the binary and voice model and returns a speaker.
func NewPiperTTS(bin, voice string, play player) (*PiperTTS, error) {
	if bin == "" {
		bin = "piper"
	}
	path, err := exec.LookPath(bin)
	if err != nil {
		return nil, fmt.Errorf("piper binary %q not found: %w", bin, err)
	}
	if voice == "" {
		return nil, fmt.Errorf("no piper voice configured")
	}
	if _, err := os.Stat(voice); err != nil {
		return nil, fmt.Errorf("piper voice %q: %w", voice, err)
	}
	if play == nil {
		return nil, fmt.Errorf("no audio output for piper")
	}
	return &PiperTTS{bin: path, voice: voice, play: play}, nil
}

// Speak synthesizes text to a temp WAV and plays it.
func (p *PiperTTS) Speak(ctx context.Context, text string) error {
	text = strings.TrimSpace(text)
	if text == "" {
		return nil
	}
	tmp, err := os.CreateTemp("", "aispeech-tts-*.wav")
	if err != nil {
		return err
	}
	path := tmp.Name()
	tmp.Close()
	defer os.Remove(path)

	cmd := exec.CommandContext(ctx, p.bin, "--model", p.voice, "--output_file", path)
	cmd.Stdin = strings.NewReader(text)
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("piper: %w", err)
	}

	pcm, sr, err := readWAVFile(path)
	if err != nil {
		return fmt.Errorf("read piper output: %w", err)
	}
	return p.play.Play(pcm, sr)
}
