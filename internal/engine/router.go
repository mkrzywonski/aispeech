package engine

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"

	"github.com/mkrzywonski/aispeech/internal/browseraudio"
)

// BrowserDevice is the pseudo-device shown in the Microphone/Speaker dropdowns
// that routes that direction to the browser (the operator's tab) instead of a
// local device.
const BrowserDevice = "Browser"

// bridge is the browser-audio transport the router and BrowserRecorder depend on
// (satisfied by *browseraudio.Bridge). Kept as an interface so the routing
// decision can be tested without a real WebSocket.
type bridge interface {
	Play(ctx context.Context, pcm []float32, sampleRate int) error
	Stop()
	OutputConnected() bool
	StartCapture()
	StopCapture()
	Segments() <-chan browseraudio.Clip
}

// AudioRouter is the single audio surface the rest of the app uses. It wraps the
// local malgo AudioContext and the browser bridge, routing playback and capture
// to whichever backend each direction has selected. With no browser device
// selected it behaves exactly like the local AudioContext. It satisfies the same
// control interface the web layer expects, plus the player and SoundPlayer seams.
type AudioRouter struct {
	local  *AudioContext
	bridge bridge
	svc    *Service // set via bindService; used to hot-swap the capture recorder

	malgoRec   Recorder
	browserRec Recorder

	outBrowser atomic.Bool // playback -> browser tab
	inBrowser  atomic.Bool // capture  -> browser tab

	soundsDir string // custom notification sounds (WAV files); "" = built-ins only
}

// NewAudioRouter builds a router over a local audio context and a browser bridge.
func NewAudioRouter(local *AudioContext, b *browseraudio.Bridge) *AudioRouter {
	return newAudioRouter(local, b)
}

// newAudioRouter is the interface-typed constructor used by tests.
func newAudioRouter(local *AudioContext, b bridge) *AudioRouter {
	return &AudioRouter{
		local:      local,
		bridge:     b,
		malgoRec:   NewMalgoRecorder(local),
		browserRec: newBrowserRecorder(b),
	}
}

func (r *AudioRouter) bindService(svc *Service) { r.svc = svc }

// SetSoundsDir points the router at a directory of custom notification sounds.
func (r *AudioRouter) SetSoundsDir(dir string) { r.soundsDir = dir }

// Sounds lists the playable sounds (built-in plus custom WAVs).
func (r *AudioRouter) Sounds() []SoundInfo { return ListSounds(r.soundsDir) }

// InitialRecorder is the recorder the Service should start with (local mic).
func (r *AudioRouter) InitialRecorder() Recorder { return r.malgoRec }

// --- playback (player + SoundPlayer) ---

// Play routes a PCM clip to the active output backend. Gain/mute for the browser
// backend are applied browser-side; pause is honored globally here.
func (r *AudioRouter) Play(ctx context.Context, pcm []float32, sampleRate int) error {
	if r.local != nil && r.local.Paused() {
		return nil
	}
	if r.outBrowser.Load() && r.bridge.OutputConnected() {
		return r.bridge.Play(ctx, pcm, sampleRate)
	}
	if r.local != nil {
		return r.local.Play(ctx, pcm, sampleRate)
	}
	return nil
}

// PlaySound plays a built-in sound (or a custom WAV of the same name from the
// sounds dir), or an explicit WAV file, via the active output.
func (r *AudioRouter) PlaySound(ctx context.Context, name, file string) (string, error) {
	// A named sound prefers a custom override in the sounds dir.
	if file == "" && name != "" {
		if pcm, rate, ok := resolveSound(r.soundsDir, name); ok {
			if err := r.Play(ctx, pcm, rate); err != nil {
				return "", err
			}
			return name, nil
		}
	}
	pcm, rate, label, err := decodeSound(name, file)
	if err != nil {
		return "", err
	}
	if err := r.Play(ctx, pcm, rate); err != nil {
		return "", err
	}
	return label, nil
}

// --- device selection (web.AudioControl) ---

// PlaybackDevices lists local output devices plus the browser option.
func (r *AudioRouter) PlaybackDevices() []string {
	if r.local == nil {
		return []string{BrowserDevice}
	}
	return append(r.local.PlaybackDevices(), BrowserDevice)
}

// CaptureDevices lists local input devices plus the browser option.
func (r *AudioRouter) CaptureDevices() []string {
	if r.local == nil {
		return []string{BrowserDevice}
	}
	return append(r.local.CaptureDevices(), BrowserDevice)
}

// SetOutputDevice selects the playback backend. The browser CLAIM (which tab)
// is done by the web layer from the request's browser session.
func (r *AudioRouter) SetOutputDevice(name string) {
	if name == BrowserDevice {
		r.outBrowser.Store(true)
		return
	}
	r.outBrowser.Store(false)
	if r.local != nil {
		r.local.SetOutputDevice(name)
	}
}

// SetInputDevice selects the capture backend and hot-swaps the Service recorder.
func (r *AudioRouter) SetInputDevice(name string) {
	if name == BrowserDevice {
		r.inBrowser.Store(true)
		if r.svc != nil {
			r.svc.SetRecorder(r.browserRec)
		}
		return
	}
	r.inBrowser.Store(false)
	if r.local != nil {
		r.local.SetInputDevice(name)
	}
	if r.svc != nil {
		r.svc.SetRecorder(r.malgoRec)
	}
}

// OutputIsBrowser / InputIsBrowser report the selected backend per direction.
func (r *AudioRouter) OutputIsBrowser() bool { return r.outBrowser.Load() }
func (r *AudioRouter) InputIsBrowser() bool  { return r.inBrowser.Load() }

// LocalDeviceCounts returns the number of enumerated local input/output devices
// (excluding the browser pseudo-device), for startup diagnostics.
func (r *AudioRouter) LocalDeviceCounts() (in, out int) {
	return len(r.local.CaptureDevices()), len(r.local.PlaybackDevices())
}

// --- levels / pause / tests (delegate to local; browser applies gain client-side) ---

func (r *AudioRouter) SetOutputVolume(v float64) {
	if r.local != nil {
		r.local.SetOutputVolume(v)
	}
}
func (r *AudioRouter) SetInputGain(v float64) {
	if r.local != nil {
		r.local.SetInputGain(v)
	}
}
func (r *AudioRouter) SetMuted(m bool) {
	if r.local != nil {
		r.local.SetMuted(m)
	}
}
func (r *AudioRouter) Muted() bool  { return r.local != nil && r.local.Muted() }
func (r *AudioRouter) Paused() bool { return r.local != nil && r.local.Paused() }

// SetPaused pauses playback on both backends (interrupting the browser clip too).
func (r *AudioRouter) SetPaused(p bool) {
	if r.local != nil {
		r.local.SetPaused(p)
	}
	if p && r.outBrowser.Load() {
		r.bridge.Stop()
	}
}

// TestSpeaker plays a chime through the active output backend.
func (r *AudioRouter) TestSpeaker() error {
	_, err := r.PlaySound(context.Background(), "chime", "")
	return err
}

// Mic test remains local-only (it probes a local input device).
func (r *AudioRouter) StartMicTest() error {
	if r.local == nil {
		return nil
	}
	return r.local.StartMicTest()
}
func (r *AudioRouter) StopMicTest() error {
	if r.local == nil {
		return nil
	}
	return r.local.StopMicTest()
}
func (r *AudioRouter) MicLevel() float64 {
	if r.local == nil {
		return 0
	}
	return r.local.MicLevel()
}
func (r *AudioRouter) MicTestActive() bool { return r.local != nil && r.local.MicTestActive() }

// BrowserRecorder implements Recorder using capture clips assembled by the
// browser bridge. Start tells the browser to begin capturing and forwards each
// assembled utterance as a Segment; Stop tells it to stop.
type BrowserRecorder struct {
	bridge bridge

	mu   sync.Mutex
	stop chan struct{}
}

// NewBrowserRecorder builds a recorder backed by the browser bridge.
func NewBrowserRecorder(b *browseraudio.Bridge) *BrowserRecorder { return newBrowserRecorder(b) }

func newBrowserRecorder(b bridge) *BrowserRecorder { return &BrowserRecorder{bridge: b} }

// Start requests browser capture and returns a channel of utterance Segments.
func (r *BrowserRecorder) Start(ctx context.Context) (<-chan Segment, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.stop != nil {
		return nil, fmt.Errorf("recorder already started")
	}
	stop := make(chan struct{})
	r.stop = stop
	out := make(chan Segment, 4)
	r.bridge.StartCapture()
	go func() {
		defer close(out)
		defer r.bridge.StopCapture()
		src := r.bridge.Segments()
		for {
			select {
			case <-ctx.Done():
				return
			case <-stop:
				return
			case clip := <-src:
				select {
				case out <- Segment{PCM: clip.PCM, SampleRate: clip.SampleRate}:
				case <-ctx.Done():
					return
				case <-stop:
					return
				}
			}
		}
	}()
	return out, nil
}

// Stop ends browser capture.
func (r *BrowserRecorder) Stop() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.stop != nil {
		close(r.stop)
		r.stop = nil
	}
	return nil
}
