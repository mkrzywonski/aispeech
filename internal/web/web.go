// Package web serves the local control UI and its JSON API.
package web

import (
	"embed"
	"encoding/json"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/mkrzywonski/aispeech/internal/authz"
	"github.com/mkrzywonski/aispeech/internal/config"
	"github.com/mkrzywonski/aispeech/internal/engine"
	"github.com/mkrzywonski/aispeech/internal/mcpinstall"
	"github.com/mkrzywonski/aispeech/internal/modelstore"
	"github.com/mkrzywonski/aispeech/internal/session"
)

//go:embed assets/index.html
var assets embed.FS

// AudioControl adjusts audio devices and levels (satisfied by *engine.AudioContext).
type AudioControl interface {
	CaptureDevices() []string
	PlaybackDevices() []string
	SetInputDevice(string)
	SetOutputDevice(string)
	SetOutputVolume(float64)
	SetInputGain(float64)
	SetMuted(bool)
	Muted() bool
	SetPaused(bool)
	Paused() bool
	TestSpeaker() error
	StartMicTest() error
	StopMicTest() error
	MicLevel() float64
	MicTestActive() bool
}

// Deps are the collaborators a Controls needs.
type Deps struct {
	Audio      AudioControl // may be nil when no audio backend
	Svc        *engine.Service
	Models     *engine.ModelManager
	Store      *modelstore.Store
	Downloader *modelstore.Downloader
	MCPURL     string // the HTTP MCP endpoint agents connect to
	Cfg        *config.Config
	Save       func() error
}

// Controls applies audio and model setting changes and persists them to config.
type Controls struct {
	mu     sync.Mutex
	audio  AudioControl
	svc    *engine.Service
	models *engine.ModelManager
	store  *modelstore.Store
	dl     *modelstore.Downloader
	mcpURL string
	cfg    *config.Config
	save   func() error
}

// NewControls binds the collaborators that settings changes act on.
func NewControls(d Deps) *Controls {
	return &Controls{
		audio:  d.Audio,
		svc:    d.Svc,
		models: d.Models,
		store:  d.Store,
		dl:     d.Downloader,
		mcpURL: d.MCPURL,
		cfg:    d.Cfg,
		save:   d.Save,
	}
}

// --- interaction (PTT dialog timeout) ---

func (c *Controls) interactionState() map[string]any {
	c.mu.Lock()
	defer c.mu.Unlock()
	return map[string]any{"dialog_timeout_minutes": float64(c.cfg.DialogTimeoutSeconds) / 60.0}
}

func (c *Controls) applyInteraction(minutes float64) error {
	if minutes < 0.25 {
		minutes = 0.25
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	secs := int(minutes*60 + 0.5)
	c.cfg.DialogTimeoutSeconds = secs
	c.svc.SetDialogTimeout(time.Duration(secs) * time.Second)
	return c.save()
}

// --- agent install ---

func (c *Controls) installState() map[string]any {
	return map[string]any{"url": c.mcpURL, "agents": mcpinstall.Statuses(c.mcpURL)}
}

// InstalledVoices lists installed piper voice paths (for per-session selection).
func (c *Controls) InstalledVoices() []string {
	return c.store.Installed(modelstore.Piper)
}

func (c *Controls) modelOptions() engine.ModelOptions {
	return engine.ModelOptions{
		WhisperBin:   c.cfg.WhisperBin,
		WhisperModel: c.cfg.WhisperModel,
		Language:     c.cfg.Language,
		PiperBin:     c.cfg.PiperBin,
		PiperVoice:   c.cfg.PiperVoice,
	}
}

type catalogView struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Size      string `json:"size"`
	Kind      string `json:"kind"`
	Installed bool   `json:"installed"`
}

type modelsResp struct {
	engine.ModelStatus
	ModelsDir        string                    `json:"models_dir"`
	InstalledWhisper []string                  `json:"installed_whisper"`
	InstalledPiper   []string                  `json:"installed_piper"`
	Catalog          []catalogView             `json:"catalog"`
	Download         modelstore.DownloadState  `json:"download"`
}

func (c *Controls) modelsState() modelsResp {
	c.mu.Lock()
	defer c.mu.Unlock()
	resp := modelsResp{
		ModelStatus:      c.models.Status(c.modelOptions()),
		ModelsDir:        c.store.Dir(),
		InstalledWhisper: c.store.Installed(modelstore.Whisper),
		InstalledPiper:   c.store.Installed(modelstore.Piper),
		Download:         c.dl.Status(),
	}
	for _, e := range modelstore.Catalog() {
		resp.Catalog = append(resp.Catalog, catalogView{
			ID: e.ID, Name: e.Name, Size: e.Size, Kind: string(e.Kind),
			Installed: c.store.IsInstalled(e),
		})
	}
	return resp
}

// startDownload downloads a catalog entry and, on completion, configures it as
// the active model/voice and hot-reloads the engine.
func (c *Controls) startDownload(id string) error {
	e, ok := modelstore.FindEntry(id)
	if !ok {
		return errorString("unknown model id")
	}
	return c.dl.Start(c.store, e, func(primary string) {
		c.mu.Lock()
		defer c.mu.Unlock()
		if e.Kind == modelstore.Whisper {
			c.cfg.WhisperModel = primary
		} else {
			c.cfg.PiperVoice = primary
		}
		_ = c.save()
		c.models.Apply(c.modelOptions())
	})
}

func (c *Controls) downloadStatus() modelstore.DownloadState { return c.dl.Status() }

type modelsUpdate struct {
	WhisperModel *string `json:"whisper_model"`
	PiperVoice   *string `json:"piper_voice"`
	WhisperBin   *string `json:"whisper_bin"`
	PiperBin     *string `json:"piper_bin"`
	Language     *string `json:"language"`
}

func (c *Controls) applyModels(u modelsUpdate) (engine.ModelStatus, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if u.WhisperModel != nil {
		c.cfg.WhisperModel = *u.WhisperModel
	}
	if u.PiperVoice != nil {
		c.cfg.PiperVoice = *u.PiperVoice
	}
	if u.WhisperBin != nil {
		c.cfg.WhisperBin = *u.WhisperBin
	}
	if u.PiperBin != nil {
		c.cfg.PiperBin = *u.PiperBin
	}
	if u.Language != nil {
		c.cfg.Language = *u.Language
	}
	if err := c.save(); err != nil {
		return c.models.Status(c.modelOptions()), err
	}
	return c.models.Apply(c.modelOptions()), nil
}

type audioState struct {
	Available    bool     `json:"available"`
	Input        []string `json:"input"`
	Output       []string `json:"output"`
	InputDevice  string   `json:"input_device"`
	OutputDevice string   `json:"output_device"`
	Volume       float64  `json:"volume"`
	Gain         float64  `json:"gain"`
	Muted        bool     `json:"muted"`
	Paused       bool     `json:"paused"`
}

func (c *Controls) state() audioState {
	c.mu.Lock()
	defer c.mu.Unlock()
	st := audioState{
		InputDevice:  c.cfg.InputDevice,
		OutputDevice: c.cfg.OutputDevice,
		Volume:       c.cfg.OutputVolume,
		Gain:         c.cfg.InputGain,
		Muted:        c.cfg.Muted,
	}
	if c.audio != nil {
		st.Available = true
		st.Input = c.audio.CaptureDevices()
		st.Output = c.audio.PlaybackDevices()
		st.Paused = c.audio.Paused()
	}
	return st
}

type audioUpdate struct {
	InputDevice  *string  `json:"input_device"`
	OutputDevice *string  `json:"output_device"`
	Volume       *float64 `json:"volume"`
	Gain         *float64 `json:"gain"`
	Muted        *bool    `json:"muted"`
}

func (c *Controls) apply(u audioUpdate) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if u.InputDevice != nil {
		c.cfg.InputDevice = *u.InputDevice
		if c.audio != nil {
			c.audio.SetInputDevice(*u.InputDevice)
		}
	}
	if u.OutputDevice != nil {
		c.cfg.OutputDevice = *u.OutputDevice
		if c.audio != nil {
			c.audio.SetOutputDevice(*u.OutputDevice)
		}
	}
	if u.Volume != nil {
		c.cfg.OutputVolume = *u.Volume
		if c.audio != nil {
			c.audio.SetOutputVolume(*u.Volume)
		}
	}
	if u.Gain != nil {
		c.cfg.InputGain = *u.Gain
		if c.audio != nil {
			c.audio.SetInputGain(*u.Gain)
		}
	}
	if u.Muted != nil {
		c.cfg.Muted = *u.Muted
		if c.audio != nil {
			c.audio.SetMuted(*u.Muted)
		}
	}
	return c.save()
}

func (c *Controls) setPaused(p bool) {
	if c.audio != nil {
		c.audio.SetPaused(p)
	}
}

var errNoAudio = errorString("no audio backend available")

type errorString string

func (e errorString) Error() string { return string(e) }

func (c *Controls) testSpeaker() error {
	if c.audio == nil {
		return errNoAudio
	}
	return c.audio.TestSpeaker()
}

func (c *Controls) startMicTest() error {
	if c.audio == nil {
		return errNoAudio
	}
	return c.audio.StartMicTest()
}

func (c *Controls) stopMicTest() error {
	if c.audio == nil {
		return errNoAudio
	}
	return c.audio.StopMicTest()
}

func (c *Controls) micLevel() (level float64, active bool) {
	if c.audio == nil {
		return 0, false
	}
	return c.audio.MicLevel(), c.audio.MicTestActive()
}

const cookieName = "aispeech_ui"

// Server wires the UI and control API to the registry and engine service.
type Server struct {
	reg      *session.Registry
	svc      *engine.Service
	controls *Controls
	store    *authz.Store
	allowed  map[string]bool
	devInj   bool
}

// New returns a web Server. devInject enables the dev-only transcript injection
// endpoint used to exercise routing without a microphone.
func New(reg *session.Registry, svc *engine.Service, controls *Controls, store *authz.Store, allowed map[string]bool, devInject bool) *Server {
	return &Server{reg: reg, svc: svc, controls: controls, store: store, allowed: allowed, devInj: devInject}
}

// Routes registers the UI and API handlers on mux. Mutating routes are wrapped
// in guard, which requires a same-origin request from a known browser session.
func (s *Server) Routes(mux *http.ServeMux) {
	g := s.guard
	mux.HandleFunc("GET /{$}", s.index)
	mux.HandleFunc("GET /api/state", s.state)
	mux.HandleFunc("POST /api/pair/token", g(s.pairToken))
	mux.HandleFunc("GET /api/audio", s.audioGet)
	mux.HandleFunc("POST /api/audio", g(s.audioSet))
	mux.HandleFunc("POST /api/audio/test-speaker", g(s.testSpeaker))
	mux.HandleFunc("POST /api/audio/pause", g(s.pauseSpeaking))
	mux.HandleFunc("POST /api/mic-test/start", g(s.micTestStart))
	mux.HandleFunc("POST /api/mic-test/stop", g(s.micTestStop))
	mux.HandleFunc("GET /api/mic-test/level", s.micTestLevel)
	mux.HandleFunc("GET /api/models", s.modelsGet)
	mux.HandleFunc("POST /api/models", g(s.modelsSet))
	mux.HandleFunc("POST /api/models/download", g(s.modelsDownload))
	mux.HandleFunc("GET /api/models/download", s.modelsDownloadStatus)
	mux.HandleFunc("GET /api/interaction", s.interactionGet)
	mux.HandleFunc("POST /api/interaction", g(s.interactionSet))
	mux.HandleFunc("GET /api/install", s.installGet)
	mux.HandleFunc("POST /api/install", g(s.installSet))
	mux.HandleFunc("POST /api/session/focus", g(s.focus))
	mux.HandleFunc("POST /api/session/rename", g(s.rename))
	mux.HandleFunc("POST /api/session/voice", g(s.sessionVoice))
	mux.HandleFunc("POST /api/session/revoke", g(s.revoke))
	mux.HandleFunc("POST /api/ptt/start", g(s.pttStart))
	mux.HandleFunc("POST /api/ptt/stop", g(s.pttStop))
	mux.HandleFunc("POST /api/listen/constant", g(s.constant))
	if s.devInj {
		mux.HandleFunc("POST /api/dev/inject", g(s.devInject))
	}
}

// guard requires a same-origin request carrying a known browser-session cookie.
// This blocks cross-origin web pages (Origin mismatch, SameSite-stripped cookie)
// from driving voice or settings. It is not a defense against a local process
// that scripts the browser flow — see the threat model in the design docs.
func (s *Server) guard(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !authz.OriginAllowed(r.Header.Get("Origin"), s.allowed) {
			http.Error(w, "bad origin", http.StatusForbidden)
			return
		}
		c, err := r.Cookie(cookieName)
		if err != nil || !s.store.KnownBrowser(c.Value) {
			http.Error(w, "no browser session", http.StatusForbidden)
			return
		}
		h(w, r)
	}
}

func (s *Server) index(w http.ResponseWriter, r *http.Request) {
	// Establish a browser-session principal for this tab/profile if absent.
	if c, err := r.Cookie(cookieName); err != nil || !s.store.KnownBrowser(c.Value) {
		http.SetCookie(w, &http.Cookie{
			Name:     cookieName,
			Value:    s.store.NewBrowser(),
			Path:     "/",
			HttpOnly: true,
			SameSite: http.SameSiteStrictMode,
		})
	}
	b, err := assets.ReadFile("assets/index.html")
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write(b)
}

// pairToken issues a single-use pairing token for the current browser session.
func (s *Server) pairToken(w http.ResponseWriter, r *http.Request) {
	c, _ := r.Cookie(cookieName) // guard guarantees a valid cookie
	tok, ok := s.store.IssueToken(c.Value)
	if !ok {
		http.Error(w, "no browser session", http.StatusForbidden)
		return
	}
	writeJSON(w, map[string]any{"token": tok, "expires_in_seconds": int(authz.DefaultTokenTTL.Seconds())})
}

type stateResp struct {
	Sessions    []session.SessionView `json:"sessions"`
	Notices     []session.Notice      `json:"notices"`
	Transcripts []session.Transcript  `json:"transcripts"`
	MicMode              string        `json:"mic_mode"`
	STTReady             bool          `json:"stt_ready"`
	TTSReady             bool          `json:"tts_ready"`
	DialogTimeoutMinutes float64       `json:"dialog_timeout_minutes"`
	Voices               []string      `json:"voices"`
	DevInject            bool          `json:"dev_inject"`
}

func (s *Server) state(w http.ResponseWriter, r *http.Request) {
	views, notices, transcripts := s.reg.Snapshot()
	writeJSON(w, stateResp{
		Sessions:             views,
		Notices:              notices,
		Transcripts:          transcripts,
		MicMode:              s.svc.Mode().String(),
		STTReady:             s.svc.STTReady(),
		TTSReady:             s.svc.TTSReady(),
		DialogTimeoutMinutes: s.svc.DialogTimeout().Minutes(),
		Voices:               s.controls.InstalledVoices(),
		DevInject:            s.devInj,
	})
}

func (s *Server) audioGet(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, s.controls.state())
}

func (s *Server) audioSet(w http.ResponseWriter, r *http.Request) {
	var u audioUpdate
	if !readJSON(w, r, &u) {
		return
	}
	if err := s.controls.apply(u); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, s.controls.state())
}

func (s *Server) testSpeaker(w http.ResponseWriter, r *http.Request) {
	// Play in the background so a ~0.5s tone doesn't tie up the request.
	go func() {
		if err := s.controls.testSpeaker(); err != nil {
			slog.Warn("test speaker", "err", err)
		}
	}()
	writeJSON(w, map[string]bool{"ok": true})
}

func (s *Server) pauseSpeaking(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Paused bool `json:"paused"`
	}
	if !readJSON(w, r, &body) {
		return
	}
	s.controls.setPaused(body.Paused)
	writeJSON(w, map[string]bool{"ok": true})
}

func (s *Server) micTestStart(w http.ResponseWriter, r *http.Request) {
	if err := s.controls.startMicTest(); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, map[string]bool{"ok": true})
}

func (s *Server) micTestStop(w http.ResponseWriter, r *http.Request) {
	if err := s.controls.stopMicTest(); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, map[string]bool{"ok": true})
}

func (s *Server) micTestLevel(w http.ResponseWriter, r *http.Request) {
	level, active := s.controls.micLevel()
	writeJSON(w, map[string]any{"level": level, "active": active})
}

func (s *Server) modelsGet(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, s.controls.modelsState())
}

func (s *Server) modelsSet(w http.ResponseWriter, r *http.Request) {
	var u modelsUpdate
	if !readJSON(w, r, &u) {
		return
	}
	if _, err := s.controls.applyModels(u); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, s.controls.modelsState())
}

func (s *Server) modelsDownload(w http.ResponseWriter, r *http.Request) {
	var body struct {
		ID string `json:"id"`
	}
	if !readJSON(w, r, &body) {
		return
	}
	if err := s.controls.startDownload(body.ID); err != nil {
		http.Error(w, err.Error(), http.StatusConflict)
		return
	}
	writeJSON(w, s.controls.downloadStatus())
}

func (s *Server) modelsDownloadStatus(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, s.controls.downloadStatus())
}

func (s *Server) interactionGet(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, s.controls.interactionState())
}

func (s *Server) interactionSet(w http.ResponseWriter, r *http.Request) {
	var body struct {
		DialogTimeoutMinutes float64 `json:"dialog_timeout_minutes"`
	}
	if !readJSON(w, r, &body) {
		return
	}
	if err := s.controls.applyInteraction(body.DialogTimeoutMinutes); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, s.controls.interactionState())
}

func (s *Server) installGet(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, s.controls.installState())
}

func (s *Server) installSet(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Agent  string `json:"agent"`
		Action string `json:"action"` // install | uninstall
	}
	if !readJSON(w, r, &body) {
		return
	}
	var err error
	if body.Action == "uninstall" {
		err = mcpinstall.Uninstall(body.Agent)
	} else {
		err = mcpinstall.Install(body.Agent, s.controls.mcpURL)
	}
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, s.controls.installState())
}

func (s *Server) focus(w http.ResponseWriter, r *http.Request) {
	var body struct {
		ID string `json:"id"`
	}
	if !readJSON(w, r, &body) {
		return
	}
	if err := s.reg.SetFocus(body.ID); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	writeJSON(w, map[string]bool{"ok": true})
}

func (s *Server) rename(w http.ResponseWriter, r *http.Request) {
	var body struct {
		ID   string `json:"id"`
		Name string `json:"name"`
	}
	if !readJSON(w, r, &body) {
		return
	}
	if err := s.reg.Rename(body.ID, body.Name); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	writeJSON(w, map[string]bool{"ok": true})
}

func (s *Server) sessionVoice(w http.ResponseWriter, r *http.Request) {
	var body struct {
		ID    string `json:"id"`
		Voice string `json:"voice"`
	}
	if !readJSON(w, r, &body) {
		return
	}
	s.reg.SetVoice(body.ID, body.Voice)
	writeJSON(w, map[string]bool{"ok": true})
}

func (s *Server) revoke(w http.ResponseWriter, r *http.Request) {
	var body struct {
		ID string `json:"id"`
	}
	if !readJSON(w, r, &body) {
		return
	}
	s.reg.Detach(body.ID)
	writeJSON(w, map[string]bool{"ok": true})
}

func (s *Server) pttStart(w http.ResponseWriter, r *http.Request) {
	if err := s.svc.StartDialog(); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, map[string]bool{"ok": true})
}

func (s *Server) pttStop(w http.ResponseWriter, r *http.Request) {
	s.svc.Stop()
	writeJSON(w, map[string]bool{"ok": true})
}

func (s *Server) constant(w http.ResponseWriter, r *http.Request) {
	var body struct {
		On bool `json:"on"`
	}
	if !readJSON(w, r, &body) {
		return
	}
	var err error
	if body.On {
		err = s.svc.StartConstant()
	} else {
		s.svc.Stop()
	}
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, map[string]bool{"ok": true})
}

// devInject simulates a transcribed utterance for routing tests. This is the
// "inject text" surface the threat model (DESIGN §7) forbids in production, so
// it is registered only when the --dev-inject flag is set. Keep it that way;
// never enable it by default, and prefer compiling it out of release builds.
func (s *Server) devInject(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Text string `json:"text"`
	}
	if !readJSON(w, r, &body) {
		return
	}
	s.svc.InjectTranscript(body.Text)
	writeJSON(w, map[string]bool{"ok": true})
}

// --- helpers ---

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

func readJSON(w http.ResponseWriter, r *http.Request, v any) bool {
	if err := json.NewDecoder(r.Body).Decode(v); err != nil {
		http.Error(w, "bad request: "+err.Error(), http.StatusBadRequest)
		return false
	}
	return true
}
