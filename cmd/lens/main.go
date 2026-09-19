package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/aster-sunfell/lens/internal/config"
	"github.com/aster-sunfell/lens/internal/gateway"
	"github.com/aster-sunfell/lens/internal/vision"
)

var (
	version = "dev"
	commit  = "unknown"
	date    = "unknown"
)

func main() {
	configPath := flag.String("config", ".env", "path to the environment configuration file")
	showVersion := flag.Bool("version", false, "print version information and exit")
	flag.Usage = func() {
		_, _ = fmt.Fprintln(flag.CommandLine.Output(), "Usage: lens [options]")
		_, _ = fmt.Fprintln(flag.CommandLine.Output(), "\nOptions:")
		flag.PrintDefaults()
	}
	flag.Parse()
	if *showVersion {
		fmt.Printf("lens %s\ncommit %s\nbuilt %s\n", version, commit, date)
		return
	}

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))
	cfg, err := config.Load(*configPath)
	if err != nil {
		logger.Error("configuration error", "error", err)
		os.Exit(1)
	}

	transport := &http.Transport{
		Proxy:                 http.ProxyFromEnvironment,
		DialContext:           (&net.Dialer{Timeout: 15 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          100,
		MaxIdleConnsPerHost:   20,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: time.Second,
		DisableCompression:    true,
	}
	visionHTTPClient := &http.Client{Transport: transport.Clone(), Timeout: cfg.VisionTimeout}
	textHTTPClient := &http.Client{Transport: transport.Clone()}
	visionClient := vision.NewClient(cfg, visionHTTPClient)
	handler := gateway.New(cfg, visionClient, textHTTPClient, logger)

	server := &http.Server{
		Addr:              cfg.ListenAddr,
		Handler:           handler,
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       120 * time.Second,
		MaxHeaderBytes:    1 << 20,
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := server.Shutdown(shutdownCtx); err != nil {
			logger.Error("graceful shutdown failed", "error", err)
		}
	}()

	logger.Info("lens listening",
		"address", cfg.ListenAddr,
		"text_host", cfg.TextBaseURL.Host,
		"vision_host", cfg.VisionBaseURL.Host,
	)
	if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		logger.Error("server stopped", "error", err)
		os.Exit(1)
	}
}
