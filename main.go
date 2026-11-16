// showmelogs - a lightweight test-result archive and browser built with the
// Go standard library only.
//
// Entry point: CLI flags (with same-named env vars as defaults), HTTP routing,
// Basic Auth middleware, access logging, idle memory reaping, graceful shutdown.
package main

import (
	"context"
	"crypto/subtle"
	"embed"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"runtime/debug"
	"strconv"
	"sync/atomic"
	"syscall"
	"time"
)

// NOTE: embed.FS is used instead of []byte because the Red Hat Go 1.26.10
// toolchain on this machine has a defect: a //go:embed directive on a
// []byte/string variable does not mark the embed package as used, which
// fails compilation with "embed imported and not used".
//
//go:embed web/index.html
var embeddedFS embed.FS

// App holds the runtime configuration.
type App struct {
	root           string // canonical absolute root directory
	dev            bool   // dev mode: read web/index.html from disk on every request
	followSymlinks bool   // resolve symlinks inside root (targets must stay inside root)
	user           string // Basic Auth username
	pass           string // Basic Auth password (non-empty enables site-wide auth)
}

// envDefault returns the value of env var key, or def when unset.
func envDefault(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func envDefaultBool(key string, def bool) bool {
	v := os.Getenv(key)
	if v == "" {
		return def
	}
	b, err := strconv.ParseBool(v)
	if err != nil {
		return def
	}
	return b
}

const (
	idleFreeInterval = 5 * time.Minute
	idleFreeAfter    = 2 * time.Minute
)

// lastRequest records the time of the most recent HTTP request (UnixNano).
var lastRequest atomic.Int64

// startIdleMemReaper periodically returns idle memory to the OS via
// debug.FreeOSMemory. With Go's default MADV_FREE the kernel keeps freed pages
// resident while the system is idle, so RSS can look much larger than the real
// footprint (live heap is typically only a few MB). This keeps reported RSS
// close to actual usage once the server has been idle for a while; busy
// periods are never interrupted by the forced GC.
func startIdleMemReaper() {
	go func() {
		t := time.NewTicker(idleFreeInterval)
		defer t.Stop()
		for range t.C {
			last := time.Unix(0, lastRequest.Load())
			if time.Since(last) >= idleFreeAfter {
				debug.FreeOSMemory()
				log.Printf("returned idle memory to OS (last request %s ago)",
					time.Since(last).Round(time.Minute))
			}
		}
	}()
}

func main() {
	fs := flag.NewFlagSet("smlog", flag.ExitOnError)
	rootFlag := fs.String("root", envDefault("SMLOG_ROOT", "/data/test_results"),
		"test results root directory")
	addrFlag := fs.String("addr", envDefault("SMLOG_ADDR", "0.0.0.0:8080"),
		"listen address (host:port)")
	userFlag := fs.String("user", envDefault("SMLOG_USER", ""),
		"Basic Auth username (auth is only enabled when -pass is set)")
	passFlag := fs.String("pass", envDefault("SMLOG_PASS", ""),
		"Basic Auth password (setting this enables site-wide auth)")
	devFlag := fs.Bool("dev", envDefaultBool("SMLOG_DEV", false),
		"serve web/index.html from disk on every request (UI development)")
	followFlag := fs.Bool("follow-symlinks", envDefaultBool("SMLOG_FOLLOW_SYMLINKS", true),
		"resolve symlinks inside the root (targets must stay inside the root)")
	fs.Parse(os.Args[1:])

	absRoot, err := filepath.Abs(*rootFlag)
	if err != nil {
		log.Fatalf("invalid -root %q: %v", *rootFlag, err)
	}
	fi, err := os.Stat(absRoot)
	if err != nil {
		if os.IsNotExist(err) {
			log.Fatalf("root directory %q does not exist.\n"+
				"Please create it first, e.g.:\n"+
				"    mkdir -p %s\n"+
				"or point -root (env SMLOG_ROOT) to an existing directory.", absRoot, absRoot)
		}
		log.Fatalf("cannot access root directory %q: %v", absRoot, err)
	}
	if !fi.IsDir() {
		log.Fatalf("root %q is not a directory", absRoot)
	}
	canonical, err := filepath.EvalSymlinks(absRoot)
	if err != nil {
		canonical = absRoot
	}

	user := *userFlag
	if *passFlag != "" && user == "" {
		user = "admin"
	}

	app := &App{
		root:           canonical,
		dev:            *devFlag,
		followSymlinks: *followFlag,
		user:           user,
		pass:           *passFlag,
	}

	srv := &http.Server{
		Addr:              *addrFlag,
		Handler:           app.routes(),
		ReadHeaderTimeout: 10 * time.Second,
		// No ReadTimeout/WriteTimeout: large downloads and streaming ZIPs can
		// take a long time.
		IdleTimeout: 120 * time.Second,
	}

	startIdleMemReaper()

	log.Printf("showmelogs listening on http://%s (root=%s, dev=%v, follow-symlinks=%v, auth=%v)",
		*addrFlag, app.root, app.dev, app.followSymlinks, app.pass != "")

	go func() {
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("server error: %v", err)
		}
	}()

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)
	<-stop
	log.Println("shutting down...")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = srv.Shutdown(ctx)
}

// routes wires all HTTP routes and middleware (auth -> access logging).
func (a *App) routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/", a.handleIndex)
	mux.HandleFunc("/api/list", a.handleList)
	mux.HandleFunc("/api/search", a.handleSearch)
	mux.HandleFunc("/api/preview", a.handlePreview)
	mux.HandleFunc("/api/download", a.handleDownload)
	return a.logging(a.auth(mux))
}

// handleIndex serves the single-page frontend (from disk in -dev mode,
// from the embedded FS otherwise).
func (a *App) handleIndex(w http.ResponseWriter, r *http.Request) {
	if !requireGET(w, r) {
		return
	}
	if r.URL.Path != "/" {
		writeAPIError(w, errNotFound("not found"))
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if a.dev {
		b, err := os.ReadFile(filepath.Join("web", "index.html"))
		if err != nil {
			writeAPIError(w, fmt.Errorf("dev mode: cannot read web/index.html (run from project root): %w", err))
			return
		}
		_, _ = w.Write(b)
		return
	}
	b, err := embeddedFS.ReadFile("web/index.html")
	if err != nil {
		writeAPIError(w, fmt.Errorf("cannot read embedded index: %w", err))
		return
	}
	_, _ = w.Write(b)
}

// auth enables site-wide Basic Auth when -pass is set.
func (a *App) auth(next http.Handler) http.Handler {
	if a.pass == "" {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		u, p, ok := r.BasicAuth()
		userOK := subtle.ConstantTimeCompare([]byte(u), []byte(a.user)) == 1
		passOK := subtle.ConstantTimeCompare([]byte(p), []byte(a.pass)) == 1
		if !ok || !userOK || !passOK {
			w.Header().Set("WWW-Authenticate", `Basic realm="showmelogs", charset="UTF-8"`)
			writeAPIError(w, errUnauthorized("authentication required"))
			return
		}
		next.ServeHTTP(w, r)
	})
}

type statusWriter struct {
	http.ResponseWriter
	status int
}

func (s *statusWriter) WriteHeader(code int) {
	if s.status == 0 {
		s.status = code
	}
	s.ResponseWriter.WriteHeader(code)
}

func (s *statusWriter) Write(b []byte) (int, error) {
	if s.status == 0 {
		s.status = http.StatusOK
	}
	return s.ResponseWriter.Write(b)
}

// logging prints a concise access log line and tracks the last request time
// for the idle memory reaper.
func (a *App) logging(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		lastRequest.Store(time.Now().UnixNano())
		sw := &statusWriter{ResponseWriter: w}
		start := time.Now()
		next.ServeHTTP(sw, r)
		log.Printf("%s %s %s -> %d (%s)",
			r.RemoteAddr, r.Method, r.URL.RequestURI(), sw.status, time.Since(start).Round(time.Millisecond))
	})
}

// requireGET rejects non-GET requests with 405.
func requireGET(w http.ResponseWriter, r *http.Request) bool {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", "GET")
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
		return false
	}
	return true
}
