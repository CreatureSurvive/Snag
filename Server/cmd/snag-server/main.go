// Command snag-server receives Snag reports, shows them in a dashboard, and forwards them to
// GitHub Issues, Discord, Slack, a webhook or email.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
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
		case "backup":
			os.Exit(runBackup(os.Args[2:]))
		case "restore":
			os.Exit(runRestore(os.Args[2:]))
		}
	}

	server.Version = version
	logger := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
	cfg, err := server.LoadConfig(os.Getenv)
	if err != nil {
		logger.Error("configuration", "err", err)
		os.Exit(2)
	}
	release, err := server.LockDataDir(cfg.DataDir)
	if err != nil {
		logger.Error("data directory", "dir", cfg.DataDir, "err", err)
		os.Exit(1)
	}
	defer release()
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

	if cfg.S3 != nil {
		// Media still on disk (S3 was down, or just turned on) moves over in the background.
		go func() {
			ticker := time.NewTicker(10 * time.Minute)
			defer ticker.Stop()
			for {
				if n := srv.OffloadPending(ctx); n > 0 {
					logger.Info("moved attachments to S3", "reports", n)
				}
				select {
				case <-ctx.Done():
					return
				case <-ticker.C:
				}
			}
		}()
	}
	if cfg.BackupDir != "" && cfg.BackupHours > 0 {
		go func() {
			ticker := time.NewTicker(time.Duration(cfg.BackupHours) * time.Hour)
			defer ticker.Stop()
			for {
				select {
				case <-ctx.Done():
					return
				case <-ticker.C:
					if path, err := server.RunScheduledBackup(cfg); err != nil {
						logger.Error("scheduled backup", "err", err)
					} else {
						logger.Info("backup written", "path", path)
					}
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

// runBackup writes a backup of SNAG_DATA_DIR to a file or stdout. It's safe while the server runs.
func runBackup(args []string) int {
	fs := flag.NewFlagSet("backup", flag.ExitOnError)
	out := fs.String("o", "-", "output file (- for stdout)")
	fs.Usage = func() {
		fmt.Fprintln(os.Stderr, "usage: snag-server backup [-o file.tar.gz]")
		fs.PrintDefaults()
	}
	_ = fs.Parse(args)
	cfg, err := server.LoadConfig(os.Getenv)
	if err != nil {
		fmt.Fprintln(os.Stderr, "configuration:", err)
		return 2
	}
	var w io.Writer = os.Stdout
	if *out != "-" {
		f, err := os.OpenFile(*out, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o640)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
		defer f.Close()
		w = f
	}
	if err := server.WriteBackup(cfg.DataDir, w); err != nil {
		fmt.Fprintln(os.Stderr, "backup:", err)
		return 1
	}
	return 0
}

// runRestore loads a backup or project export into SNAG_DATA_DIR. The server must be stopped.
func runRestore(args []string) int {
	fs := flag.NewFlagSet("restore", flag.ExitOnError)
	merge := fs.Bool("merge", false, "add the backup's projects to the existing data")
	replace := fs.Bool("replace", false, "move the existing data into <data>/replaced-<time>/ first")
	fs.Usage = func() {
		fmt.Fprintln(os.Stderr, "usage: snag-server restore [--merge | --replace] file.tar.gz (- for stdin)")
		fs.PrintDefaults()
	}
	_ = fs.Parse(args)
	if fs.NArg() != 1 || (*merge && *replace) {
		fs.Usage()
		return 2
	}
	cfg, err := server.LoadConfig(os.Getenv)
	if err != nil {
		fmt.Fprintln(os.Stderr, "configuration:", err)
		return 2
	}
	release, err := server.LockDataDir(cfg.DataDir)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	defer release()
	var r io.Reader = os.Stdin
	if name := fs.Arg(0); name != "-" {
		f, err := os.Open(name)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
		defer f.Close()
		r = f
	}
	summary, err := server.Restore(cfg, r, server.RestoreOptions{Merge: *merge, Replace: *replace})
	if err != nil {
		fmt.Fprintln(os.Stderr, "restore:", err)
		return 1
	}
	fmt.Println("restored", summary)
	return 0
}
