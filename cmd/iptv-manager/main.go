package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"golang.org/x/crypto/bcrypt"
	"iptv-manager/internal/media"
	"iptv-manager/internal/resolver"
	"iptv-manager/internal/store"
	"iptv-manager/internal/subscription"
	"iptv-manager/internal/web"
)

var version = "0.4.1"

func env(key, fallback string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return fallback
}

func main() {
	showVersion := flag.Bool("version", false, "print version")
	check := flag.Bool("healthcheck", false, "check local HTTP service")
	flag.Parse()
	if *showVersion {
		fmt.Println(version)
		return
	}
	if *check {
		_, port, err := net.SplitHostPort(env("LISTEN_ADDR", "127.0.0.1:9000"))
		if err != nil {
			os.Exit(1)
		}
		client := http.Client{Timeout: 3 * time.Second}
		resp, err := client.Get("http://127.0.0.1:" + port + "/healthz")
		if err != nil {
			os.Exit(1)
		}
		resp.Body.Close()
		if resp.StatusCode != 200 {
			os.Exit(1)
		}
		return
	}
	if err := run(); err != nil {
		slog.Error("服务启动失败", "reason", err.Error())
		os.Exit(1)
	}
}

func run() error {
	var level slog.Level
	if err := level.UnmarshalText([]byte(env("LOG_LEVEL", "info"))); err != nil {
		return errors.New("LOG_LEVEL 必须为 debug、info、warn 或 error")
	}
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level})))
	db, err := store.Open(filepath.Join(env("DATA_DIR", "./data"), "iptv-manager.db"))
	if err != nil {
		return fmt.Errorf("打开 SQLite: %w", err)
	}
	defer db.Close()
	ctx := context.Background()
	hash, err := db.AdminHash(ctx)
	if err != nil {
		return err
	}
	if hash == "" {
		password := os.Getenv("ADMIN_PASSWORD")
		if file := os.Getenv("ADMIN_PASSWORD_FILE"); file != "" {
			content, err := os.ReadFile(file)
			if err != nil {
				return errors.New("无法读取 ADMIN_PASSWORD_FILE")
			}
			password = strings.TrimRight(string(content), "\r\n")
		}
		if len(password) < 12 || len(password) > 72 || strings.Contains(password, "CHANGE_ME") {
			return errors.New("首次启动请设置 ADMIN_PASSWORD（12–72 字节）或 ADMIN_PASSWORD_FILE")
		}
		encoded, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
		if err != nil {
			return err
		}
		if err = db.SetAdminHash(ctx, string(encoded)); err != nil {
			return err
		}
		slog.Info("管理员密码已初始化；后续请在管理页面修改")
	}
	settings, err := db.Settings(ctx)
	if err != nil {
		return err
	}
	if settings.BaseURL == "" && os.Getenv("PUBLIC_URL") != "" {
		settings.BaseURL = os.Getenv("PUBLIC_URL")
		if err = db.SaveSettings(ctx, settings); err != nil {
			return fmt.Errorf("PUBLIC_URL 无效: %w", err)
		}
	}
	secure := strings.HasPrefix(settings.BaseURL, "https://")
	if raw := os.Getenv("SECURE_COOKIES"); raw != "" && raw != "auto" {
		secure, err = strconv.ParseBool(raw)
		if err != nil {
			return errors.New("SECURE_COOKIES 必须为 auto、true 或 false")
		}
	}
	cacheMB, err := strconv.Atoi(env("CACHE_MB", "32"))
	if err != nil || cacheMB < 0 || cacheMB > 128 {
		return errors.New("CACHE_MB 必须在 0–128 之间")
	}
	res := resolver.New(resolver.Options{Command: env("YTDLP_BIN", "yt-dlp"), CookiesFile: os.Getenv("COOKIES_FILE"), JSRuntime: env("JS_RUNTIME", "node"), Timeout: 45 * time.Second})
	streams := media.New(db, res, media.Options{CacheBytes: int64(cacheMB) * 1024 * 1024, MaxFetches: 8})
	subs := subscription.New(db, streams.Invalidate)
	admin, err := web.New(db, streams, web.Options{SecureCookies: secure, Version: version, SyncSubscription: subs.Sync})
	if err != nil {
		return err
	}
	mux := http.NewServeMux()
	mux.Handle("/playlist.m3u", streams.Handler())
	mux.Handle("/watch/", streams.Handler())
	mux.Handle("/media/", streams.Handler())
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		if _, err := db.Settings(r.Context()); err != nil {
			http.Error(w, "unavailable", 503)
			return
		}
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		fmt.Fprintln(w, "ok")
	})
	mux.Handle("/", admin)
	server := &http.Server{Addr: env("LISTEN_ADDR", "127.0.0.1:9000"), Handler: mux, ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 15 * time.Second, WriteTimeout: 90 * time.Second, IdleTimeout: 60 * time.Second, MaxHeaderBytes: 16 * 1024}
	listener, err := net.Listen("tcp", server.Addr)
	if err != nil {
		return err
	}
	signals, stop := signal.NotifyContext(ctx, syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	syncCtx, syncCancel := context.WithCancel(signals)
	syncDone := make(chan struct{})
	go func() { defer close(syncDone); subs.Run(syncCtx) }()
	defer func() { syncCancel(); <-syncDone }()
	done := make(chan error, 1)
	go func() { done <- server.Serve(listener) }()
	slog.Info("IPTV Manager 已启动", "listen", server.Addr, "version", version, "cache_mb", cacheMB)
	ticker := time.NewTicker(15 * time.Second)
	defer ticker.Stop()
	var serveErr error
	running := true
	for running {
		select {
		case <-ticker.C:
			flushCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
			err := streams.FlushTraffic(flushCtx)
			cancel()
			if err != nil {
				slog.Warn("流量统计暂存失败，将重试")
			}
		case <-signals.Done():
			running = false
		case serveErr = <-done:
			running = false
		}
	}
	shutdown, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if err := server.Shutdown(shutdown); err != nil {
		server.Close()
	}
	flushCtx, flushCancel := context.WithTimeout(ctx, 5*time.Second)
	defer flushCancel()
	if err := streams.FlushTraffic(flushCtx); err != nil {
		slog.Warn("退出时流量统计未完全保存")
	}
	if serveErr != nil && !errors.Is(serveErr, http.ErrServerClosed) {
		return serveErr
	}
	return nil
}
