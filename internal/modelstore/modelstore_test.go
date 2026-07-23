package modelstore

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestDownloadInstallsAndReportsProgress(t *testing.T) {
	payload := make([]byte, 4096)
	for i := range payload {
		payload[i] = byte(i)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", itoa(len(payload)))
		if r.Method == http.MethodHead {
			return
		}
		_, _ = w.Write(payload)
	}))
	defer srv.Close()

	dir := t.TempDir()
	store := New(dir)
	entry := CatalogEntry{
		ID: "test", Name: "Test", Kind: Whisper,
		Files: []FileSpec{{URL: srv.URL + "/model.bin", Name: "model.bin"}},
	}

	if store.IsInstalled(entry) {
		t.Fatal("should not be installed yet")
	}

	done := make(chan string, 1)
	dl := &Downloader{}
	if err := dl.Start(store, entry, func(p string) { done <- p }); err != nil {
		t.Fatalf("start: %v", err)
	}

	select {
	case p := <-done:
		if p != filepath.Join(dir, "model.bin") {
			t.Fatalf("primary path = %q", p)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("download did not complete")
	}

	got, err := os.ReadFile(filepath.Join(dir, "model.bin"))
	if err != nil {
		t.Fatalf("read installed: %v", err)
	}
	if len(got) != len(payload) {
		t.Fatalf("installed size = %d, want %d", len(got), len(payload))
	}
	if !store.IsInstalled(entry) {
		t.Fatal("entry should be installed")
	}
	st := dl.Status()
	if !st.Done || st.Received != int64(len(payload)) {
		t.Fatalf("status = %+v", st)
	}
	// No leftover .part file.
	if _, err := os.Stat(filepath.Join(dir, "model.bin.part")); !os.IsNotExist(err) {
		t.Fatal(".part file should be gone")
	}
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}
