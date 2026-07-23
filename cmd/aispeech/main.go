// Command aispeech runs the local voice hub: an MCP server plus a browser UI
// that give AI agents speak() and listen() capabilities.
package main

import (
	"context"
	"flag"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"time"

	"github.com/mkrzywonski/aispeech/internal/config"
	"github.com/mkrzywonski/aispeech/internal/engine"
	"github.com/mkrzywonski/aispeech/internal/mcpserver"
	"github.com/mkrzywonski/aispeech/internal/modelstore"
	"github.com/mkrzywonski/aispeech/internal/session"
	"github.com/mkrzywonski/aispeech/internal/web"
)

const version = "0.0.1"

func main() {
	// Subcommand: stdio↔HTTP MCP bridge spawned by an AI client.
	if len(os.Args) > 1 && os.Args[1] == "mcp-proxy" {
		os.Exit(proxyMain(os.Args[2:]))
	}

	var (
		addr      = flag.String("addr", "", "override bind address (default from config, e.g. 127.0.0.1:7071)")
		devInject = flag.Bool("dev-inject", false, "enable the dev-only transcript injection endpoint (routing tests without a mic)")
	)
	flag.Parse()

	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo})))

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
	web.New(reg, svc, controls, *devInject).Routes(mux)
	mux.Handle("/mcp", mcpserver.NewHandler(reg, svc, mcpserver.Options{
		DefaultListenTimeout: time.Duration(cfg.DialogTimeoutSeconds) * time.Second,
		MaxListenTimeout:     10 * time.Minute,
	}))

	srv := &http.Server{Addr: bindAddr, Handler: mux}

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
