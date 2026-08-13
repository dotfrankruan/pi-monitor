package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/dotfrankruan/pi-monitor/internal/metrics"
	"github.com/dotfrankruan/pi-monitor/internal/monitor"
	"github.com/dotfrankruan/pi-monitor/internal/storage"
	webui "github.com/dotfrankruan/pi-monitor/internal/web"
)

var version = "dev"

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "pi-monitor:", err)
		os.Exit(1)
	}
}

func run() error {
	var (
		listen          = flag.String("listen", ":49152", "HTTP listen address")
		dataDir         = flag.String("data-dir", "./data", "SQLite and Parquet storage directory")
		diskPath        = flag.String("disk-path", "/", "filesystem path to monitor")
		sampleInterval  = flag.Duration("sample-interval", 500*time.Millisecond, "metric sample interval")
		flushInterval   = flag.Duration("flush-interval", time.Hour, "SQLite batch flush interval")
		persistInterval = flag.Duration("persist-interval", 5*time.Second, "durable SQLite history resolution")
		showVersion     = flag.Bool("version", false, "print version and exit")
		verbose         = flag.Bool("verbose", false, "enable debug logs")
	)
	flag.Parse()
	if *showVersion {
		fmt.Println(version)
		return nil
	}
	if *sampleInterval < 100*time.Millisecond {
		return errors.New("sample-interval must be at least 100ms")
	}
	if *flushInterval < time.Second {
		return errors.New("flush-interval must be at least 1s")
	}
	if *persistInterval < *sampleInterval {
		return errors.New("persist-interval must be at least sample-interval")
	}
	absDataDir, err := filepath.Abs(*dataDir)
	if err != nil {
		return err
	}
	level := slog.LevelInfo
	if *verbose {
		level = slog.LevelDebug
	}
	logger := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: level}))
	store, err := storage.Open(absDataDir)
	if err != nil {
		return fmt.Errorf("open storage: %w", err)
	}
	defer store.Close()
	removed, err := store.PreparePersistence(context.Background(), *persistInterval)
	if err != nil {
		return fmt.Errorf("prepare persistent history: %w", err)
	}

	collector := metrics.NewCollector("", "", *diskPath)
	mon := monitor.New(collector, store, monitor.Config{SampleInterval: *sampleInterval, FlushInterval: *flushInterval, PersistenceInterval: *persistInterval}, logger)
	if removed > 0 {
		logger.Info("compacted existing SQLite history", "removed_rows", removed, "resolution", *persistInterval)
	}
	httpServer := &http.Server{
		Addr:              *listen,
		Handler:           webui.New(mon, collector.SystemInfo(), logger).Handler(),
		ReadHeaderTimeout: 5 * time.Second,
		IdleTimeout:       2 * time.Minute,
	}
	signalCtx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	ctx, cancel := context.WithCancel(signalCtx)
	defer cancel()

	monitorDone := make(chan error, 1)
	serverDone := make(chan error, 1)
	go func() { monitorDone <- mon.Run(ctx) }()
	go func() {
		logger.Info("Pi Monitor started", "version", version, "listen", *listen, "data_dir", absDataDir,
			"sample_interval", *sampleInterval, "flush_interval", *flushInterval, "persist_interval", *persistInterval)
		serverDone <- httpServer.ListenAndServe()
	}()

	select {
	case <-signalCtx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		serverErr := httpServer.Shutdown(shutdownCtx)
		// Monitor.Run receives the same signal and flushes the pending in-memory
		// batch. Wait for that flush before the deferred database close.
		monitorErr := <-monitorDone
		return errors.Join(serverErr, monitorErr)
	case err := <-serverDone:
		if errors.Is(err, http.ErrServerClosed) || err == nil {
			return nil
		}
		cancel()
		<-monitorDone
		return err
	case err := <-monitorDone:
		cancel()
		shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer shutdownCancel()
		return errors.Join(err, httpServer.Shutdown(shutdownCtx))
	}
}
