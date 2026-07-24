package browseraudio

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/coder/websocket"
)

// serveBridge starts a bridge behind an httptest server and dials a client that
// plays the browser role. It returns the bridge, the client conn, and cleanup.
func serveBridge(t *testing.T) (*Bridge, *websocket.Conn, func()) {
	t.Helper()
	b := New()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ws, err := websocket.Accept(w, r, &websocket.AcceptOptions{InsecureSkipVerify: true})
		if err != nil {
			return
		}
		_ = b.Serve(r.Context(), ws, "browser1")
	}))
	ctx := context.Background()
	c, _, err := websocket.Dial(ctx, "ws"+srv.URL[len("http"):], nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	// Wait for the server to register the connection.
	deadline := time.Now().Add(2 * time.Second)
	for b.latest("browser1") == nil {
		if time.Now().After(deadline) {
			t.Fatal("bridge never registered the connection")
		}
		time.Sleep(5 * time.Millisecond)
	}
	return b, c, func() { c.Close(websocket.StatusNormalClosure, ""); srv.Close() }
}

func TestPlaybackRoundTrip(t *testing.T) {
	b, client, cleanup := serveBridge(t)
	defer cleanup()
	if !b.ClaimOutput("browser1") {
		t.Fatal("ClaimOutput failed")
	}

	want := []float32{0.1, -0.2, 0.3, -0.4}
	var got []float32
	clientDone := make(chan struct{})
	go func() {
		defer close(clientDone)
		ctx := context.Background()
		// Expect a "play" control frame, then a binary clip; ack it.
		_, ctrl, err := client.Read(ctx)
		if err != nil {
			return
		}
		var m msg
		json.Unmarshal(ctrl, &m)
		if m.Type != "play" {
			t.Errorf("first frame type = %q, want play", m.Type)
		}
		_, bin, err := client.Read(ctx)
		if err != nil {
			return
		}
		got = bytesToFloats(bin)
		client.Write(ctx, websocket.MessageText, mustJSON(msg{Type: "played", ID: m.ID}))
	}()

	if err := b.Play(context.Background(), want, SampleRate); err != nil {
		t.Fatalf("Play: %v", err)
	}
	<-clientDone
	if len(got) != len(want) {
		t.Fatalf("received %d samples, want %d", len(got), len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("sample %d = %v, want %v", i, got[i], want[i])
		}
	}
}

func TestPlaybackDroppedWhenNoOutputClaimed(t *testing.T) {
	b, _, cleanup := serveBridge(t)
	defer cleanup()
	// No ClaimOutput: Play must return immediately without blocking on an ack.
	done := make(chan error, 1)
	go func() { done <- b.Play(context.Background(), []float32{1, 2, 3}, SampleRate) }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Play with no output = %v, want nil", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Play blocked with no output claimed")
	}
}

func TestCaptureAssembly(t *testing.T) {
	b, client, cleanup := serveBridge(t)
	defer cleanup()
	if !b.ClaimInput("browser1") {
		t.Fatal("ClaimInput failed")
	}

	ctx := context.Background()
	want := []float32{0.5, 0.6, 0.7}
	client.Write(ctx, websocket.MessageText, mustJSON(msg{Type: "utt-start"}))
	client.Write(ctx, websocket.MessageBinary, floatsToBytes(want))
	client.Write(ctx, websocket.MessageText, mustJSON(msg{Type: "utt-end"}))

	select {
	case clip := <-b.Segments():
		if len(clip.PCM) != len(want) || clip.SampleRate != SampleRate {
			t.Fatalf("clip = %+v, want %d samples @ %d", clip, len(want), SampleRate)
		}
		for i := range want {
			if clip.PCM[i] != want[i] {
				t.Fatalf("sample %d = %v, want %v", i, clip.PCM[i], want[i])
			}
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no capture clip assembled")
	}
}

func TestCaptureIgnoredFromUnclaimedTab(t *testing.T) {
	b, client, cleanup := serveBridge(t)
	defer cleanup()
	// Input not claimed: binary audio must be ignored (no clip emitted).
	ctx := context.Background()
	client.Write(ctx, websocket.MessageText, mustJSON(msg{Type: "utt-start"}))
	client.Write(ctx, websocket.MessageBinary, floatsToBytes([]float32{1, 2}))
	client.Write(ctx, websocket.MessageText, mustJSON(msg{Type: "utt-end"}))
	select {
	case clip := <-b.Segments():
		t.Fatalf("unexpected clip from unclaimed tab: %+v", clip)
	case <-time.After(300 * time.Millisecond):
		// good: nothing emitted
	}
}

func mustJSON(m msg) []byte {
	b, _ := json.Marshal(m)
	return b
}
