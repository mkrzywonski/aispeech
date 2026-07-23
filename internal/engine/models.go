package engine

import (
	"os"
	"os/exec"
)

// ModelOptions names the STT/TTS binaries and models to load.
type ModelOptions struct {
	WhisperBin   string
	WhisperModel string
	Language     string
	PiperBin     string
	PiperVoice   string
}

// ModelStatus reports what is configured, what resolves, and what is live.
type ModelStatus struct {
	WhisperBin     string `json:"whisper_bin"`
	WhisperBinOK   bool   `json:"whisper_bin_ok"`
	WhisperModel   string `json:"whisper_model"`
	WhisperModelOK bool   `json:"whisper_model_ok"`
	WhisperError   string `json:"whisper_error,omitempty"`
	STTReady       bool   `json:"stt_ready"`

	PiperBin     string `json:"piper_bin"`
	PiperBinOK   bool   `json:"piper_bin_ok"`
	PiperVoice   string `json:"piper_voice"`
	PiperVoiceOK bool   `json:"piper_voice_ok"`
	PiperError   string `json:"piper_error,omitempty"`
	TTSReady     bool   `json:"tts_ready"`

	Language string `json:"language"`
	HasAudio bool   `json:"has_audio"`
}

// ModelManager loads STT/TTS engines and swaps them into a running Service.
type ModelManager struct {
	svc *Service
	ac  *AudioContext // nil when no audio backend (TTS impossible)
}

// NewModelManager returns a manager bound to a Service and audio context.
func NewModelManager(svc *Service, ac *AudioContext) *ModelManager {
	return &ModelManager{svc: svc, ac: ac}
}

// Status reports the current configuration without changing the running engines.
func (m *ModelManager) Status(o ModelOptions) ModelStatus {
	st := ModelStatus{
		WhisperBin:     orDefault(o.WhisperBin, "whisper-cli"),
		WhisperModel:   o.WhisperModel,
		PiperBin:       orDefault(o.PiperBin, "piper"),
		PiperVoice:     o.PiperVoice,
		Language:       o.Language,
		HasAudio:       m.ac != nil,
		WhisperModelOK: fileOK(o.WhisperModel),
		PiperVoiceOK:   fileOK(o.PiperVoice),
	}
	st.WhisperBinOK = binOK(st.WhisperBin)
	st.PiperBinOK = binOK(st.PiperBin)

	// Dry-run construction to surface actionable error messages.
	if _, err := NewWhisperSTT(o.WhisperBin, o.WhisperModel, o.Language); err != nil {
		st.WhisperError = err.Error()
	}
	if m.ac == nil {
		st.PiperError = "no audio output device"
	} else if _, err := NewPiperTTS(o.PiperBin, o.PiperVoice, m.ac); err != nil {
		st.PiperError = err.Error()
	}

	st.STTReady = m.svc.STTReady()
	st.TTSReady = m.svc.TTSReady()
	return st
}

// Apply loads the engines from o and swaps them into the Service (installing
// null engines where construction fails), returning the resulting status.
func (m *ModelManager) Apply(o ModelOptions) ModelStatus {
	if w, err := NewWhisperSTT(o.WhisperBin, o.WhisperModel, o.Language); err == nil {
		m.svc.SetTranscriber(w)
	} else {
		m.svc.SetTranscriber(NullTranscriber{})
	}

	if m.ac == nil {
		m.svc.SetSpeaker(NullSpeaker{})
	} else if p, err := NewPiperTTS(o.PiperBin, o.PiperVoice, m.ac); err == nil {
		m.svc.SetSpeaker(p)
	} else {
		m.svc.SetSpeaker(NullSpeaker{})
	}

	return m.Status(o)
}

func binOK(bin string) bool {
	_, err := exec.LookPath(bin)
	return err == nil
}

func fileOK(p string) bool {
	if p == "" {
		return false
	}
	fi, err := os.Stat(p)
	return err == nil && !fi.IsDir()
}

func orDefault(v, d string) string {
	if v == "" {
		return d
	}
	return v
}
