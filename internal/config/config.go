// Package config loads and persists aispeech settings.
package config

import (
	"encoding/json"
	"os"
	"path/filepath"
)

// Config is the persisted application configuration.
type Config struct {
	// Network.
	Addr string `json:"addr"` // MCP + UI bind address, e.g. 127.0.0.1:7071

	// Audio devices (empty = system default).
	InputDevice  string `json:"input_device"`
	OutputDevice string `json:"output_device"`

	// STT.
	WhisperBin   string `json:"whisper_bin"`   // path to whisper.cpp binary
	WhisperModel string `json:"whisper_model"` // path to a ggml model
	Language     string `json:"language"`      // e.g. "en", "auto"

	// TTS.
	PiperBin   string `json:"piper_bin"`   // path to piper binary
	PiperVoice string `json:"piper_voice"` // path to a piper .onnx voice

	// ModelsDir is where downloaded models are stored ("" = default data dir).
	ModelsDir string `json:"models_dir"`

	// Levels (0..1 multipliers).
	OutputVolume float64 `json:"output_volume"` // TTS playback gain
	InputGain    float64 `json:"input_gain"`    // microphone gain
	Muted        bool    `json:"muted"`         // silence playback (persists across restarts)

	// Interaction.
	DialogTimeoutSeconds int `json:"dialog_timeout_seconds"` // PTT dialog idle timeout
	SpeakCharCap         int `json:"speak_char_cap"`         // hard cap on speak() text
}

// Default returns a config populated with sensible defaults.
func Default() Config {
	return Config{
		Addr:                 "127.0.0.1:7071",
		Language:             "en",
		WhisperBin:           "whisper-cli",
		PiperBin:             "piper",
		OutputVolume:         1.0,
		InputGain:            1.0,
		DialogTimeoutSeconds: 180,
		SpeakCharCap:         600,
	}
}

// Path returns the on-disk config path (~/.config/aispeech/config.json).
func Path() (string, error) {
	dir, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "aispeech", "config.json"), nil
}

// DefaultModelsDir returns the default download location for models
// (~/.local/share/aispeech/models, honoring XDG_DATA_HOME).
func DefaultModelsDir() string {
	base := os.Getenv("XDG_DATA_HOME")
	if base == "" {
		if home, err := os.UserHomeDir(); err == nil {
			base = filepath.Join(home, ".local", "share")
		} else if cfg, err := os.UserConfigDir(); err == nil {
			base = cfg // fallback for non-Linux
		}
	}
	return filepath.Join(base, "aispeech", "models")
}

// ResolvedModelsDir returns ModelsDir or the default when unset.
func (c Config) ResolvedModelsDir() string {
	if c.ModelsDir != "" {
		return c.ModelsDir
	}
	return DefaultModelsDir()
}

// Load reads config from disk, returning defaults (merged) if none exists.
func Load() (Config, error) {
	cfg := Default()
	p, err := Path()
	if err != nil {
		return cfg, err
	}
	b, err := os.ReadFile(p)
	if err != nil {
		if os.IsNotExist(err) {
			return cfg, nil // first run: defaults
		}
		return cfg, err
	}
	// Unmarshal over defaults so new fields keep their default values.
	if err := json.Unmarshal(b, &cfg); err != nil {
		return cfg, err
	}
	return cfg, nil
}

// Save writes config to disk, creating the directory as needed.
func (c Config) Save() error {
	p, err := Path()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return err
	}
	b, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(p, b, 0o600)
}
