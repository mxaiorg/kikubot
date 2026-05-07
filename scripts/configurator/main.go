// Configurator is an HTMX-based dashboard for editing kikubot's configuration
// files (configs/env/common.env, configs/env/<agent>.env, and the bundled
// docker-mailserver postfix maps under services/dms/config/).
//
// Run from the kikubot project root:
//
//	go run ./scripts/configurator
//
// Or point at any deployment:
//
//	go run ./scripts/configurator -root /path/to/kikubot -port 50042
package main

import (
	"context"
	"embed"
	"errors"
	"flag"
	"fmt"
	"html/template"
	"io/fs"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"
)

//go:embed templates/*.html
var templatesFS embed.FS

//go:embed static/*
var staticFS embed.FS

type server struct {
	root   string
	tmpls  map[string]*template.Template
	static fs.FS
}

func main() {
	root := flag.String("root", ".", "path to the kikubot project root")
	port := flag.Int("port", 50042, "port to listen on")
	addr := flag.String("addr", "127.0.0.1", "address to bind on (use 0.0.0.0 to expose externally)")
	flag.Usage = func() {
		fmt.Fprintf(os.Stderr, "Agent Configurator — HTMX dashboard for kikubot\n\nUsage:\n  %s [flags]\n\nFlags:\n", filepath.Base(os.Args[0]))
		flag.PrintDefaults()
	}
	flag.Parse()

	abs, err := filepath.Abs(*root)
	if err != nil {
		fatal("resolving root: %v", err)
	}
	if info, err := os.Stat(abs); err != nil || !info.IsDir() {
		fatal("root path %q is not a directory", abs)
	}

	tmpls, err := buildTemplates()
	if err != nil {
		fatal("loading templates: %v", err)
	}
	static, err := fs.Sub(staticFS, "static")
	if err != nil {
		fatal("loading static assets: %v", err)
	}

	s := &server{root: abs, tmpls: tmpls, static: static}
	mux := http.NewServeMux()
	mux.HandleFunc("/", s.handleHome)
	mux.HandleFunc("/agents/defaults", s.handleDefaults)
	mux.HandleFunc("/agents/list", s.handleAgentsList)
	mux.HandleFunc("/agents/new", s.handleAgentNew)
	mux.HandleFunc("/agents/edit", s.handleAgentEdit)
	mux.HandleFunc("/agents/save", s.handleAgentSave)
	mux.HandleFunc("/email-service", s.handleEmailService)
	mux.Handle("/static/", http.StripPrefix("/static/", http.FileServer(http.FS(static))))

	httpSrv := &http.Server{
		Addr:              fmt.Sprintf("%s:%d", *addr, *port),
		Handler:           withLogging(mux),
		ReadHeaderTimeout: 5 * time.Second,
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	go func() {
		log.Printf("Agent Configurator listening on http://%s — root=%s", httpSrv.Addr, abs)
		if err := httpSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Fatalf("server: %v", err)
		}
	}()

	<-ctx.Done()
	log.Println("shutdown requested")
	shutCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = httpSrv.Shutdown(shutCtx)
}

func fatal(format string, args ...any) {
	fmt.Fprintf(os.Stderr, format+"\n", args...)
	os.Exit(2)
}

// buildTemplates parses each page (which itself includes the layout) into its
// own *template.Template, keyed by page name. We re-define `page` per file,
// so they cannot share a single tree.
func buildTemplates() (map[string]*template.Template, error) {
	pages := []string{"home", "defaults", "agent_form", "agents_list", "email_service"}
	files := map[string]string{
		"home":          "templates/home.html",
		"defaults":      "templates/defaults.html",
		"agent_form":    "templates/agent_form.html",
		"agents_list":   "templates/agents_list.html",
		"email_service": "templates/email_service.html",
	}
	out := make(map[string]*template.Template, len(pages))
	for _, name := range pages {
		t := template.New(name).Funcs(templateFuncs)
		t, err := t.ParseFS(templatesFS, "templates/layout.html", files[name])
		if err != nil {
			return nil, fmt.Errorf("parsing %s: %w", name, err)
		}
		out[name] = t
	}
	return out, nil
}

// withLogging adds a single-line access log per request.
func withLogging(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w, status: 200}
		h.ServeHTTP(rec, r)
		log.Printf("%s %s %d %s", r.Method, r.URL.Path, rec.status, time.Since(start))
	})
}

type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (r *statusRecorder) WriteHeader(code int) {
	r.status = code
	r.ResponseWriter.WriteHeader(code)
}
