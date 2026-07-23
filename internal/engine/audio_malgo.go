package engine

import (
	"context"
	"encoding/binary"
	"fmt"
	"math"
	"path/filepath"
	"runtime"
	"sync"
	"sync/atomic"
	"unsafe"

	"github.com/gen2brain/malgo"
)

const captureRate = 16000 // whisper.cpp expects 16 kHz mono

// AudioContext owns the miniaudio backend context, shared by capture and
// playback. Device selection and levels are adjustable at runtime.
type AudioContext struct {
	ctx *malgo.AllocatedContext

	mu         sync.Mutex // guards device-name selections and micDev
	inputName  string
	outputName string
	micDev     *malgo.Device // active mic-test capture device, if any

	outVol   atomic.Uint64 // float64 bits: playback gain
	inGain   atomic.Uint64 // float64 bits: capture gain
	micLevel atomic.Uint64 // float64 bits: latest mic-test RMS (0..1)
	muted    atomic.Bool   // silences playback (persisted; independent of volume)
	paused   atomic.Bool   // suppress playback entirely; cuts current, holds future

	playMu   sync.Mutex // guards the active playback device
	playDev  *malgo.Device
	playStop chan struct{}
}

// NewAudioContext initializes the audio backend. It fails when no backend/device
// is available (e.g. a headless sandbox), letting callers fall back to null
// engines.
func NewAudioContext() (*AudioContext, error) {
	ctx, err := malgo.InitContext(nil, malgo.ContextConfig{}, func(string) {})
	if err != nil {
		return nil, err
	}
	a := &AudioContext{ctx: ctx}
	a.SetOutputVolume(1.0)
	a.SetInputGain(1.0)
	return a, nil
}

// SetInputDevice selects the capture device by name (""=system default). Takes
// effect on the next listen start.
func (a *AudioContext) SetInputDevice(name string) {
	a.mu.Lock()
	a.inputName = name
	a.mu.Unlock()
}

// SetOutputDevice selects the playback device by name (""=system default).
func (a *AudioContext) SetOutputDevice(name string) {
	a.mu.Lock()
	a.outputName = name
	a.mu.Unlock()
}

// SetOutputVolume sets the playback gain (0..1+, clamped to a sane ceiling).
func (a *AudioContext) SetOutputVolume(v float64) { a.outVol.Store(math.Float64bits(clampGain(v))) }

// SetInputGain sets the capture gain.
func (a *AudioContext) SetInputGain(v float64) { a.inGain.Store(math.Float64bits(clampGain(v))) }

// SetMuted silences (or restores) playback without changing the volume level.
func (a *AudioContext) SetMuted(m bool) { a.muted.Store(m) }

// Muted reports whether playback is muted.
func (a *AudioContext) Muted() bool { return a.muted.Load() }

// SetPaused pauses or resumes speech output. Pausing cuts the current utterance
// and suppresses further playback until resumed; unlike mute, paused audio does
// not play at all. Not persisted (a transient "hold the voice" control).
func (a *AudioContext) SetPaused(p bool) {
	a.paused.Store(p)
	if p {
		a.StopPlayback()
	}
}

// Paused reports whether speech output is paused.
func (a *AudioContext) Paused() bool { return a.paused.Load() }

// StopPlayback interrupts the currently-playing sound (e.g. a TTS utterance).
func (a *AudioContext) StopPlayback() {
	a.playMu.Lock()
	if a.playStop != nil {
		close(a.playStop)
		a.playStop = nil
		a.playDev = nil
	}
	a.playMu.Unlock()
}

func (a *AudioContext) outputVolume() float64 { return math.Float64frombits(a.outVol.Load()) }
func (a *AudioContext) inputGain() float64    { return math.Float64frombits(a.inGain.Load()) }

func clampGain(v float64) float64 {
	if v < 0 {
		return 0
	}
	if v > 4 {
		return 4
	}
	return v
}

// resolveDeviceID returns the miniaudio device-id pointer for the named device,
// or nil for the system default. The returned infos slice must be kept alive
// until InitDevice has copied the id.
func (a *AudioContext) resolveDeviceID(kind malgo.DeviceType, name string) (unsafe.Pointer, []malgo.DeviceInfo) {
	if name == "" {
		return nil, nil
	}
	infos, err := a.ctx.Devices(kind)
	if err != nil {
		return nil, nil
	}
	for i := range infos {
		if infos[i].Name() == name {
			return infos[i].ID.Pointer(), infos
		}
	}
	return nil, infos // not found -> default
}

// Close releases the backend.
func (a *AudioContext) Close() {
	if a.ctx != nil {
		_ = a.ctx.Uninit()
		a.ctx.Free()
		a.ctx = nil
	}
}

// CaptureDevices lists input device names.
func (a *AudioContext) CaptureDevices() []string  { return a.deviceNames(malgo.Capture) }

// PlaybackDevices lists output device names.
func (a *AudioContext) PlaybackDevices() []string { return a.deviceNames(malgo.Playback) }

func (a *AudioContext) deviceNames(kind malgo.DeviceType) []string {
	infos, err := a.ctx.Devices(kind)
	if err != nil {
		return nil
	}
	names := make([]string, 0, len(infos))
	for i := range infos {
		names = append(names, infos[i].Name())
	}
	return names
}

// Play plays mono float32 PCM at the given sample rate, blocking until it
// finishes or is interrupted by StopPlayback. Output volume and mute are applied
// live in the callback, so changing them takes effect mid-utterance.
func (a *AudioContext) Play(pcm []float32, sampleRate int) error {
	if len(pcm) == 0 || a.paused.Load() {
		return nil // paused: drop this utterance rather than play it
	}
	// Store raw (unscaled) samples; gain/mute are applied per-callback.
	buf := make([]byte, len(pcm)*4)
	for i, s := range pcm {
		binary.LittleEndian.PutUint32(buf[i*4:], math.Float32bits(s))
	}

	a.mu.Lock()
	outName := a.outputName
	a.mu.Unlock()
	devID, keep := a.resolveDeviceID(malgo.Playback, outName)

	cfg := malgo.DefaultDeviceConfig(malgo.Playback)
	cfg.Playback.Format = malgo.FormatF32
	cfg.Playback.Channels = 1
	cfg.Playback.DeviceID = devID
	cfg.SampleRate = uint32(sampleRate)

	var (
		pos  int
		once sync.Once
		done = make(chan struct{})
	)
	cb := malgo.DeviceCallbacks{
		Data: func(out, _ []byte, frames uint32) {
			g := float32(a.outputVolume())
			if a.muted.Load() {
				g = 0
			}
			want := int(frames) * 4
			i := 0
			for ; i+4 <= want && pos+4 <= len(buf); i, pos = i+4, pos+4 {
				f := math.Float32frombits(binary.LittleEndian.Uint32(buf[pos:])) * g
				binary.LittleEndian.PutUint32(out[i:], math.Float32bits(clampF(f)))
			}
			for ; i < want && i < len(out); i++ {
				out[i] = 0
			}
			if pos >= len(buf) {
				once.Do(func() { close(done) })
			}
		},
	}
	dev, err := malgo.InitDevice(a.ctx.Context, cfg, cb)
	runtime.KeepAlive(keep)
	if err != nil {
		return err
	}
	defer dev.Uninit()

	stop := make(chan struct{})
	a.playMu.Lock()
	a.playDev, a.playStop = dev, stop
	a.playMu.Unlock()
	defer func() {
		a.playMu.Lock()
		if a.playStop == stop {
			a.playDev, a.playStop = nil, nil
		}
		a.playMu.Unlock()
	}()

	if err := dev.Start(); err != nil {
		return err
	}
	select {
	case <-done: // finished
	case <-stop: // interrupted by StopPlayback
	}
	return nil
}

// TestSpeaker plays a short chime through the selected output at current volume.
func (a *AudioContext) TestSpeaker() error {
	_, err := a.PlaySound(context.Background(), "chime", "")
	return err
}

// PlaySound plays a built-in named sound or a WAV file through the selected
// output (respecting volume/mute), returning a label for what was played.
func (a *AudioContext) PlaySound(_ context.Context, name, file string) (string, error) {
	var (
		pcm   []float32
		rate  int
		label string
	)
	switch {
	case file != "":
		p, sr, err := readWAVFile(file)
		if err != nil {
			return "", fmt.Errorf("play file: %w", err)
		}
		pcm, rate, label = p, sr, filepath.Base(file)
	case name != "":
		p, sr, ok := generateSound(name)
		if !ok {
			return "", fmt.Errorf("unknown sound %q (have: %v)", name, SoundNames())
		}
		pcm, rate, label = p, sr, name
	default:
		return "", fmt.Errorf("provide a sound name or a WAV file path")
	}
	if err := a.Play(pcm, rate); err != nil {
		return "", err
	}
	return label, nil
}

// StartMicTest opens the selected input and reports a live level via MicLevel.
func (a *AudioContext) StartMicTest() error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.micDev != nil {
		return nil // already running
	}
	devID, keep := a.resolveDeviceID(malgo.Capture, a.inputName)

	cfg := malgo.DefaultDeviceConfig(malgo.Capture)
	cfg.Capture.Format = malgo.FormatF32
	cfg.Capture.Channels = 1
	cfg.Capture.DeviceID = devID
	cfg.SampleRate = captureRate

	cb := malgo.DeviceCallbacks{
		Data: func(_, in []byte, frames uint32) {
			n := int(frames)
			if n == 0 {
				return
			}
			gain := a.inputGain()
			var sum float64
			for i := 0; i < n; i++ {
				s := float64(math.Float32frombits(binary.LittleEndian.Uint32(in[i*4:]))) * gain
				sum += s * s
			}
			a.micLevel.Store(math.Float64bits(math.Sqrt(sum / float64(n))))
		},
	}
	dev, err := malgo.InitDevice(a.ctx.Context, cfg, cb)
	runtime.KeepAlive(keep)
	if err != nil {
		return err
	}
	if err := dev.Start(); err != nil {
		dev.Uninit()
		return err
	}
	a.micDev = dev
	return nil
}

// StopMicTest closes the mic-test capture device.
func (a *AudioContext) StopMicTest() error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.micDev != nil {
		a.micDev.Uninit()
		a.micDev = nil
	}
	a.micLevel.Store(0)
	return nil
}

// MicLevel returns the latest mic-test RMS level (0..1), or 0 when not testing.
func (a *AudioContext) MicLevel() float64 { return math.Float64frombits(a.micLevel.Load()) }

// MicTestActive reports whether a mic test is running.
func (a *AudioContext) MicTestActive() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.micDev != nil
}

// MalgoRecorder captures the microphone and emits endpointed utterance segments.
type MalgoRecorder struct {
	ac *AudioContext

	mu   sync.Mutex
	dev  *malgo.Device
	stop chan struct{}
}

// NewMalgoRecorder builds a recorder on the shared audio context.
func NewMalgoRecorder(ac *AudioContext) *MalgoRecorder { return &MalgoRecorder{ac: ac} }

// Start opens the input device and returns a channel of endpointed utterances.
func (r *MalgoRecorder) Start(ctx context.Context) (<-chan Segment, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.dev != nil {
		return nil, fmt.Errorf("recorder already started")
	}

	r.ac.mu.Lock()
	inName := r.ac.inputName
	r.ac.mu.Unlock()
	devID, keep := r.ac.resolveDeviceID(malgo.Capture, inName)

	cfg := malgo.DefaultDeviceConfig(malgo.Capture)
	cfg.Capture.Format = malgo.FormatF32
	cfg.Capture.Channels = 1
	cfg.Capture.DeviceID = devID
	cfg.SampleRate = captureRate

	raw := make(chan []float32, 64) // audio thread -> VAD goroutine
	cb := malgo.DeviceCallbacks{
		Data: func(_, in []byte, frames uint32) {
			n := int(frames)
			gain := float32(r.ac.inputGain())
			s := make([]float32, n)
			for i := 0; i < n; i++ {
				s[i] = math.Float32frombits(binary.LittleEndian.Uint32(in[i*4:])) * gain
			}
			select {
			case raw <- s: // deliver to VAD
			default: // never block the audio thread; drop on backpressure
			}
		},
	}
	dev, err := malgo.InitDevice(r.ac.ctx.Context, cfg, cb)
	runtime.KeepAlive(keep)
	if err != nil {
		return nil, err
	}
	if err := dev.Start(); err != nil {
		dev.Uninit()
		return nil, err
	}
	r.dev = dev
	r.stop = make(chan struct{})

	out := make(chan Segment, 4)
	go r.process(ctx, raw, out)
	return out, nil
}

// Stop closes the input device.
func (r *MalgoRecorder) Stop() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.dev == nil {
		return nil
	}
	close(r.stop)
	r.dev.Uninit()
	r.dev = nil
	return nil
}

// process runs the VAD over incoming audio and emits Segments.
func (r *MalgoRecorder) process(ctx context.Context, raw <-chan []float32, out chan<- Segment) {
	defer close(out)
	v := newVAD()
	for {
		select {
		case <-ctx.Done():
			return
		case <-r.stop:
			return
		case s := <-raw:
			for _, utt := range v.push(s) {
				select {
				case out <- Segment{PCM: utt, SampleRate: captureRate}:
				case <-ctx.Done():
					return
				case <-r.stop:
					return
				}
			}
		}
	}
}

// vad is a simple energy-based voice-activity detector with silence hangover.
// It is intentionally dependency-free; a Silero-VAD backend is a planned upgrade.
type vad struct {
	block    []float32 // accumulates until blockSize
	inSpeech bool
	utt      []float32
	silence  int // consecutive silent blocks while in speech
}

const (
	vadBlock     = 320   // 20 ms at 16 kHz
	vadThreshold = 0.012 // RMS gate
	vadHangover  = 35    // silent blocks (~0.7s) ending an utterance
	vadMinBlocks = 8     // ignore utterances shorter than ~160 ms
	vadMaxSamp   = captureRate * 20
)

func newVAD() *vad { return &vad{} }

// push feeds samples and returns any completed utterances.
func (v *vad) push(samples []float32) [][]float32 {
	var done [][]float32
	v.block = append(v.block, samples...)
	for len(v.block) >= vadBlock {
		blk := v.block[:vadBlock]
		v.block = v.block[vadBlock:]
		speech := rms(blk) > vadThreshold

		if speech {
			v.inSpeech = true
			v.silence = 0
			v.utt = append(v.utt, blk...)
		} else if v.inSpeech {
			v.utt = append(v.utt, blk...) // keep trailing silence for context
			v.silence++
			if v.silence >= vadHangover {
				if u := v.finish(); u != nil {
					done = append(done, u)
				}
			}
		}
		if len(v.utt) >= vadMaxSamp {
			if u := v.finish(); u != nil {
				done = append(done, u)
			}
		}
	}
	return done
}

func (v *vad) finish() []float32 {
	utt := v.utt
	blocks := len(utt) / vadBlock
	v.utt = nil
	v.inSpeech = false
	v.silence = 0
	if blocks < vadMinBlocks {
		return nil
	}
	return utt
}

func rms(b []float32) float32 {
	var sum float64
	for _, s := range b {
		sum += float64(s) * float64(s)
	}
	return float32(math.Sqrt(sum / float64(len(b))))
}
