// Command aispeech runs the local voice hub: an MCP server plus a browser UI
// that give AI agents speak() and listen() capabilities.
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"runtime/debug"
	"time"

	"github.com/mkrzywonski/aispeech/internal/authz"
	"github.com/mkrzywonski/aispeech/internal/config"
	"github.com/mkrzywonski/aispeech/internal/engine"
	"github.com/mkrzywonski/aispeech/internal/mcpserver"
	"github.com/mkrzywonski/aispeech/internal/modelstore"
	"github.com/mkrzywonski/aispeech/internal/session"
	"github.com/mkrzywonski/aispeech/internal/web"
)

// version is overridable at build time via -ldflags "-X main.version=...".
var version = "dev"

// fullVersion combines the (possibly ldflag-set) version with the VCS revision
// Go embeds automatically for `go build` from a git checkout.
func fullVersion() string {
	v := version
	if bi, ok := debug.ReadBuildInfo(); ok {
		var rev, when string
		var dirty bool
		for _, s := range bi.Settings {
			switch s.Key {
			case "vcs.revision":
				rev = s.Value
			case "vcs.time":
				when = s.Value
			case "vcs.modified":
				dirty = s.Value == "true"
			}
		}
		if rev != "" {
			if len(rev) > 12 {
				rev = rev[:12]
			}
			suffix := ""
			if dirty {
				suffix = "+dirty"
			}
			v = fmt.Sprintf("%s (%s%s %s)", v, rev, suffix, when)
		}
	}
	return v
}

func main() {
	// Subcommand: stdio↔HTTP MCP bridge spawned by an AI client.
	if len(os.Args) > 1 && os.Args[1] == "mcp-proxy" {
		os.Exit(proxyMain(os.Args[2:]))
	}

	var (
		addr        = flag.String("addr", "", "override bind address (default from config, e.g. 127.0.0.1:7071)")
		devInject   = flag.Bool("dev-inject", false, "enable the dev-only transcript injection endpoint (routing tests without a mic)")
		showVersion = flag.Bool("version", false, "print version and exit")
	)
	flag.BoolVar(showVersion, "v", false, "print version and exit (shorthand)")
	flag.Parse()

	if *showVersion {
		fmt.Println("aispeech", fullVersion())
		return
	}

	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo})))
	slog.Info("aispeech starting", "version", fullVersion())

	cfg, err := config.Load()
	if err != nil {
		slog.Error("load config", "err", err)
		os.Exit(1)
	}
	bindAddr := cfg.Addr
	if *addr != "" {
		bindAddr = *addr // CLI override, not persisted
	}

	reg := session.New()

	svc, audioCtx, cleanup, warnings := engine.Build(reg, engine.BuildOptions{
		WhisperBin:    cfg.WhisperBin,
		WhisperModel:  cfg.WhisperModel,
		Language:      cfg.Language,
		PiperBin:      cfg.PiperBin,
		PiperVoice:    cfg.PiperVoice,
		DialogTimeout: time.Duration(cfg.DialogTimeoutSeconds) * time.Second,
		SpeakCap:      cfg.SpeakCharCap,
	})
	defer cleanup()
	for _, w := range warnings {
		slog.Warn(w)
	}

	// Apply persisted audio selections and levels to the backend.
	var audioControl web.AudioControl
	if audioCtx != nil {
		audioCtx.SetInputDevice(cfg.InputDevice)
		audioCtx.SetOutputDevice(cfg.OutputDevice)
		audioCtx.SetOutputVolume(cfg.OutputVolume)
		audioCtx.SetInputGain(cfg.InputGain)
		audioCtx.SetMuted(cfg.Muted)
		audioControl = audioCtx

		in, out := len(audioCtx.CaptureDevices()), len(audioCtx.PlaybackDevices())
		slog.Info("audio devices", "input", in, "output", out)
		if in == 0 && out == 0 {
			slog.Warn("no audio devices found — the ALSA/PulseAudio client libraries are likely missing from the loader path. " +
				"Run the packaged binary (`nix run .` or ./result/bin/aispeech), or add alsa-lib/libpulseaudio to LD_LIBRARY_PATH for `go run`.")
		}
	}
	models := engine.NewModelManager(svc, audioCtx)
	store := modelstore.New(cfg.ResolvedModelsDir())
	authStore := authz.NewStore(authz.DefaultTokenTTL)
	allow := authz.NewAllower(bindAddr, cfg.TrustedHosts)
	controls := web.NewControls(web.Deps{
		Audio:      audioControl,
		Svc:        svc,
		Models:     models,
		Store:      store,
		Downloader: &modelstore.Downloader{},
		MCPURL:     "http://" + bindAddr + "/mcp",
		Cfg:        &cfg,
		Save:       func() error { return cfg.Save() },
	})

	mux := http.NewServeMux()
	web.New(reg, svc, controls, authStore, allow, *devInject).Routes(mux)
	mux.Handle("/mcp", mcpserver.NewHandler(reg, svc, authStore,
		func() []string { return store.Installed(modelstore.Piper) },
		mcpserver.Options{
			Version:              fullVersion(),
			DefaultListenTimeout: time.Duration(cfg.DialogTimeoutSeconds) * time.Second,
			MaxListenTimeout:     10 * time.Minute,
		}))

	// Reject requests whose Host isn't the loopback/bound authority — the
	// primary defense against DNS-rebinding and malicious web pages.
	handler := authz.HostGuard(mux, allow)
	srv := &http.Server{Addr: bindAddr, Handler: handler}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	go func() {
		slog.Info("aispeech listening", "ui", "http://"+bindAddr, "mcp", "http://"+bindAddr+"/mcp", "dev_inject", *devInject)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			slog.Error("http server", "err", err)
			stop()
		}
	}()

	<-ctx.Done()
	slog.Info("shutting down")
	svc.Stop()
	shutCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	_ = srv.Shutdown(shutCtx)
}
