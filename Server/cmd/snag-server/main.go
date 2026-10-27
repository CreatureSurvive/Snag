// Command snag-server receives Snag reports, shows them in a dashboard, and forwards them to
// GitHub Issues, Discord, Slack, a webhook or email.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/CreatureSurvive/Snag/Server/internal/server"
)

var version = "dev"

func main() {
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "healthcheck":
			// For Docker HEALTHCHECK in images without curl.
			addr := os.Getenv("SNAG_ADDR")
			if addr == "" || addr[0] == ':' {
				addr = "127.0.0.1" + addr
				if addr == "127.0.0.1" {
					addr += ":8080"
				}
			}
			resp, err := (&http.Client{Timeout: 3 * time.Second}).Get("http://" + addr + "/api/v1/health")
			if err != nil || resp.StatusCode != http.StatusOK {
				os.Exit(1)
			}
			os.Exit(0)
		case "version", "--version":
			fmt.Println(version)
			return
		}
	}

	logger := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
	cfg, err := server.LoadConfig(os.Getenv)
	if err != nil {
		logger.Error("configuration", "err", err)
		os.Exit(2)
	}
	srv, err := server.Open(cfg, logger)
	if err != nil {
		logger.Error("opening data directory", "dir", cfg.DataDir, "err", err)
		os.Exit(1)
	}
	store := srv.Store()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go srv.Dispatcher().Run(ctx, 30*time.Second)
	// GitHub sync: webhooks bring changes in immediately; polling catches anything missed (or
	// everything, when GitHub can't reach this server).
	go func() {
		interval := 10 * time.Minute
		if cfg.GitHubWebhookSecret != "" {
			interval = time.Hour
		}
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				srv.GitHub().Poll(ctx)
			}
		}
	}()
	if retention := cfg.Retention(); retention > 0 {
		go func() {
			ticker := time.NewTicker(6 * time.Hour)
			defer ticker.Stop()
			for {
				if n := store.Prune(time.Now().Add(-retention)); n > 0 {
					logger.Info("pruned old reports", "count", n)
				}
				select {
				case <-ctx.Done():
					return
				case <-ticker.C:
				}
			}
		}()
	}

	httpServer := &http.Server{
		Addr:              cfg.Addr,
		Handler:           srv,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       5 * time.Minute,
		WriteTimeout:      5 * time.Minute,
		IdleTimeout:       2 * time.Minute,
		MaxHeaderBytes:    64 << 10,
	}
	go func() {
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		_ = httpServer.Shutdown(shutdown)
	}()

	dashboard := "enabled"
	if cfg.AdminPassword == "" {
		dashboard = "disabled (set SNAG_ADMIN_PASSWORD)"
	}
	var projects []string
	for _, p := range srv.Projects().List() {
		projects = append(projects, p.Slug)
	}
	if len(projects) == 0 {
		logger.Warn("no projects yet: create one in the dashboard, or set SNAG_API_KEYS / SNAG_PROJECTS_FILE")
	}
	logger.Info("snag-server listening", "version", version, "addr", cfg.Addr, "data", cfg.DataDir,
		"projects", projects, "dashboard", dashboard)
	if err := httpServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		logger.Error("server", "err", err)
		os.Exit(1)
	}
}
