// Package modelstore manages downloadable speech models: a curated catalog,
// on-disk installation scanning, and a progress-tracked downloader.
package modelstore

import (
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
)

// Kind distinguishes STT models from TTS voices.
type Kind string

const (
	Whisper Kind = "whisper"
	Piper   Kind = "piper"
)

// FileSpec is one downloadable file. The first file of an entry is its primary
// (the one configured as the model/voice); the rest are companions (e.g. a
// piper .onnx.json).
type FileSpec struct {
	URL  string
	Name string
}

// CatalogEntry is a downloadable model.
type CatalogEntry struct {
	ID    string
	Name  string
	Size  string
	Kind  Kind
	Files []FileSpec
}

// PrimaryName is the entry's primary filename.
func (e CatalogEntry) PrimaryName() string {
	if len(e.Files) == 0 {
		return ""
	}
	return e.Files[0].Name
}

const (
	whisperBase = "https://huggingface.co/ggerganov/whisper.cpp/resolve/main/"
	piperBase   = "https://huggingface.co/rhasspy/piper-voices/resolve/main/"
)

func whisperEntry(name, size string) CatalogEntry {
	file := "ggml-" + name + ".bin"
	return CatalogEntry{
		ID: "whisper-" + name, Name: "Whisper " + name, Size: size, Kind: Whisper,
		Files: []FileSpec{{URL: whisperBase + file, Name: file}},
	}
}

func piperEntry(id, name, size, path string) CatalogEntry {
	// path e.g. "en/en_US/lessac/medium/en_US-lessac-medium"
	base := path[strings.LastIndex(path, "/")+1:]
	onnx := base + ".onnx"
	return CatalogEntry{
		ID: "piper-" + id, Name: name, Size: size, Kind: Piper,
		Files: []FileSpec{
			{URL: piperBase + path + ".onnx", Name: onnx},
			{URL: piperBase + path + ".onnx.json", Name: onnx + ".json"},
		},
	}
}

// Catalog returns the curated set of downloadable models.
func Catalog() []CatalogEntry {
	return []CatalogEntry{
		whisperEntry("tiny.en", "75 MB"),
		whisperEntry("base.en", "142 MB"),
		whisperEntry("small.en", "466 MB"),
		whisperEntry("base", "142 MB (multilingual)"),
		whisperEntry("small", "466 MB (multilingual)"),
		piperEntry("en_US-lessac-medium", "Piper en_US · lessac (medium)", "63 MB", "en/en_US/lessac/medium/en_US-lessac-medium"),
		piperEntry("en_US-amy-medium", "Piper en_US · amy (medium)", "63 MB", "en/en_US/amy/medium/en_US-amy-medium"),
		piperEntry("en_US-ryan-high", "Piper en_US · ryan (high)", "114 MB", "en/en_US/ryan/high/en_US-ryan-high"),
		piperEntry("en_GB-alba-medium", "Piper en_GB · alba (medium)", "63 MB", "en/en_GB/alba/medium/en_GB-alba-medium"),
	}
}

// FindEntry returns the catalog entry with the given id.
func FindEntry(id string) (CatalogEntry, bool) {
	for _, e := range Catalog() {
		if e.ID == id {
			return e, true
		}
	}
	return CatalogEntry{}, false
}

// Store is a directory of installed models.
type Store struct {
	dir string
}

// New returns a Store rooted at dir (created lazily on download).
func New(dir string) *Store { return &Store{dir: dir} }

// Dir returns the store directory.
func (s *Store) Dir() string { return s.dir }

// Installed returns absolute paths of installed models of the given kind, sorted.
// Whisper: *.bin. Piper: *.onnx that have a sibling *.onnx.json.
func (s *Store) Installed(kind Kind) []string {
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		return nil
	}
	var out []string
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		switch kind {
		case Whisper:
			if strings.HasSuffix(name, ".bin") {
				out = append(out, filepath.Join(s.dir, name))
			}
		case Piper:
			if strings.HasSuffix(name, ".onnx") {
				if _, err := os.Stat(filepath.Join(s.dir, name+".json")); err == nil {
					out = append(out, filepath.Join(s.dir, name))
				}
			}
		}
	}
	sort.Strings(out)
	return out
}

// IsInstalled reports whether all of an entry's files are present.
func (s *Store) IsInstalled(e CatalogEntry) bool {
	for _, f := range e.Files {
		if _, err := os.Stat(filepath.Join(s.dir, f.Name)); err != nil {
			return false
		}
	}
	return true
}

// PrimaryPath returns the absolute path an installed entry's primary file would have.
func (s *Store) PrimaryPath(e CatalogEntry) string {
	return filepath.Join(s.dir, e.PrimaryName())
}

// DownloadState is a snapshot of the downloader.
type DownloadState struct {
	Active   bool   `json:"active"`
	ID       string `json:"id,omitempty"`
	Name     string `json:"name,omitempty"`
	Received int64  `json:"received"`
	Total    int64  `json:"total"`
	Done     bool   `json:"done"`
	Error    string `json:"error,omitempty"`
}

// Downloader runs one download at a time with progress tracking.
type Downloader struct {
	mu       sync.Mutex
	active   bool
	id       string
	name     string
	total    int64
	done     bool
	errMsg   string
	received atomic.Int64
}

// Status returns a snapshot of the current/last download.
func (d *Downloader) Status() DownloadState {
	d.mu.Lock()
	defer d.mu.Unlock()
	return DownloadState{
		Active:   d.active,
		ID:       d.id,
		Name:     d.name,
		Received: d.received.Load(),
		Total:    d.total,
		Done:     d.done,
		Error:    d.errMsg,
	}
}

// Start begins downloading an entry into store. onComplete is called with the
// primary file path on success. Returns an error if a download is in progress.
func (d *Downloader) Start(store *Store, e CatalogEntry, onComplete func(primaryPath string)) error {
	d.mu.Lock()
	if d.active {
		d.mu.Unlock()
		return fmt.Errorf("a download is already in progress")
	}
	if err := os.MkdirAll(store.dir, 0o755); err != nil {
		d.mu.Unlock()
		return err
	}
	d.active = true
	d.id = e.ID
	d.name = e.Name
	d.done = false
	d.errMsg = ""
	d.total = totalSize(e)
	d.received.Store(0)
	d.mu.Unlock()

	go func() {
		err := d.run(store, e)
		d.mu.Lock()
		d.active = false
		if err != nil {
			d.errMsg = err.Error()
		} else {
			d.done = true
		}
		d.mu.Unlock()
		if err == nil && onComplete != nil {
			onComplete(store.PrimaryPath(e))
		}
	}()
	return nil
}

func (d *Downloader) run(store *Store, e CatalogEntry) error {
	for _, f := range e.Files {
		if err := d.fetch(f.URL, filepath.Join(store.dir, f.Name)); err != nil {
			return fmt.Errorf("%s: %w", f.Name, err)
		}
	}
	return nil
}

func (d *Downloader) fetch(url, dest string) error {
	resp, err := http.Get(url) //nolint:gosec // curated catalog URLs
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("http %d", resp.StatusCode)
	}
	tmp := dest + ".part"
	out, err := os.Create(tmp)
	if err != nil {
		return err
	}
	_, err = io.Copy(out, io.TeeReader(resp.Body, counter{&d.received}))
	closeErr := out.Close()
	if err != nil {
		os.Remove(tmp)
		return err
	}
	if closeErr != nil {
		os.Remove(tmp)
		return closeErr
	}
	return os.Rename(tmp, dest) // atomic install
}

type counter struct{ n *atomic.Int64 }

func (c counter) Write(p []byte) (int, error) {
	c.n.Add(int64(len(p)))
	return len(p), nil
}

func totalSize(e CatalogEntry) int64 {
	var total int64
	for _, f := range e.Files {
		resp, err := http.Head(f.URL) //nolint:gosec // curated catalog URLs
		if err != nil {
			return 0
		}
		resp.Body.Close()
		if resp.ContentLength <= 0 {
			return 0
		}
		total += resp.ContentLength
	}
	return total
}
