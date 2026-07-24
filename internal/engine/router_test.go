package engine

import (
	"context"
	"testing"
	"time"

	"github.com/mkrzywonski/aispeech/internal/browseraudio"
)

// fakeBridge records what the router sends it, standing in for a browser tab.
type fakeBridge struct {
	connected bool
	played    [][]float32
	stops     int
	captureOn int
	segs      chan browseraudio.Clip
}

func (f *fakeBridge) Play(_ context.Context, pcm []float32, _ int) error {
	f.played = append(f.played, pcm)
	return nil
}
func (f *fakeBridge) Stop()                 { f.stops++ }
func (f *fakeBridge) OutputConnected() bool { return f.connected }
func (f *fakeBridge) StartCapture()         { f.captureOn++ }
func (f *fakeBridge) StopCapture()          { f.captureOn-- }
func (f *fakeBridge) Segments() <-chan browseraudio.Clip {
	if f.segs == nil {
		f.segs = make(chan browseraudio.Clip, 4)
	}
	return f.segs
}

func TestRouterOutputSelection(t *testing.T) {
	fb := &fakeBridge{}
	r := newAudioRouter(nil, fb) // nil local: exercise only the browser path

	// Default: browser output not selected, no local -> clip dropped.
	if err := r.Play(context.Background(), []float32{1, 2}, 16000); err != nil {
		t.Fatalf("Play (local, nil) = %v", err)
	}
	if len(fb.played) != 0 {
		t.Fatalf("clip went to browser before selection: %v", fb.played)
	}

	// Select Browser output, but no tab connected yet -> still dropped.
	r.SetOutputDevice(BrowserDevice)
	if !r.OutputIsBrowser() {
		t.Fatal("OutputIsBrowser should be true after selecting Browser")
	}
	_ = r.Play(context.Background(), []float32{1, 2}, 16000)
	if len(fb.played) != 0 {
		t.Fatalf("clip sent to a disconnected browser: %v", fb.played)
	}

	// Tab connected -> clip routes to the browser.
	fb.connected = true
	_ = r.Play(context.Background(), []float32{3, 4, 5}, 16000)
	if len(fb.played) != 1 || len(fb.played[0]) != 3 {
		t.Fatalf("clip not routed to browser: %v", fb.played)
	}
}

func TestRouterPauseStopsBrowser(t *testing.T) {
	fb := &fakeBridge{connected: true}
	r := newAudioRouter(nil, fb)
	r.SetOutputDevice(BrowserDevice)
	r.SetPaused(true) // with nil local this only needs to interrupt the browser
	if fb.stops == 0 {
		t.Fatal("pausing while browser output active should Stop the bridge")
	}
}

func TestBrowserRecorderForwardsClips(t *testing.T) {
	fb := &fakeBridge{segs: make(chan browseraudio.Clip, 4)}
	rec := newBrowserRecorder(fb)
	segs, err := rec.Start(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if fb.captureOn != 1 {
		t.Fatalf("Start should request capture, captureOn=%d", fb.captureOn)
	}
	fb.segs <- browseraudio.Clip{PCM: []float32{0.1, 0.2}, SampleRate: 16000}
	select {
	case seg := <-segs:
		if len(seg.PCM) != 2 || seg.SampleRate != 16000 {
			t.Fatalf("segment = %+v", seg)
		}
	case <-time.After(time.Second):
		t.Fatal("no segment forwarded from bridge clip")
	}
	if err := rec.Stop(); err != nil {
		t.Fatal(err)
	}
}
