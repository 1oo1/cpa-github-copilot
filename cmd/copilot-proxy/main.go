package main

import (
	"context"
	"flag"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/1oo1/cpa-github-copilot/internal/app"
	"github.com/1oo1/cpa-github-copilot/web"
)

var version = "dev"

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
func run() error {
	config := flag.String("config", "data/config.yaml", "configuration path")
	initOnly := flag.Bool("init", false, "create configuration and exit")
	importDir := flag.String("import-auth-dir", "", "copy Copilot credentials from a CPA auth directory and exit")
	showVersion := flag.Bool("version", false, "print version")
	flag.Parse()
	if *showVersion {
		fmt.Println("copilot-proxy", version)
		return nil
	}
	cfg, err := app.LoadConfig(*config)
	if err != nil {
		return err
	}
	if *initOnly {
		fmt.Println("Configuration ready:", *config)
		return nil
	}
	ui, uiErr := web.Index()
	if uiErr != nil && *importDir == "" {
		return fmt.Errorf("management UI is not built: run make build")
	}
	server, err := app.New(cfg, *config, nil, ui)
	if err != nil {
		return err
	}
	defer server.Close()
	if *importDir != "" {
		n, err := server.ImportDirectory(*importDir)
		if err != nil {
			return err
		}
		fmt.Printf("Imported %d Copilot accounts; source directory unchanged\n", n)
		return nil
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	server.Start(ctx)
	httpServer := &http.Server{Addr: cfg.Listen, Handler: server, ReadHeaderTimeout: 10 * time.Second, MaxHeaderBytes: 1 << 20}
	done := make(chan error, 1)
	go func() { done <- httpServer.ListenAndServe() }()
	fmt.Printf("Copilot Proxy %s listening on %s\n", version, cfg.Listen)
	select {
	case err := <-done:
		if err != http.ErrServerClosed {
			return fmt.Errorf("HTTP server stopped: %w", err)
		}
	case <-ctx.Done():
		shutdown, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		_ = httpServer.Shutdown(shutdown)
		_ = httpServer.Close()
	}
	return nil
}
