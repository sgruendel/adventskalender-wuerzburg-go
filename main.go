package main

import (
	"context"
	"errors"
	"fmt"
	"html/template"
	"io/fs"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"runtime"
	"syscall"
	"time"
)

func main() {
	ConfigRuntime()
	StartWorkers()
	StartServer()
}

// ConfigRuntime sets the number of operating system threads.
func ConfigRuntime() {
	nuCPU := runtime.NumCPU()
	runtime.GOMAXPROCS(nuCPU)
	fmt.Printf("Running with %d CPUs\n", nuCPU)
}

// StartWorkers start starsWorker by goroutine.
func StartWorkers() {
	go statsWorker()
}

// StartServer registers all routes and starts the HTTP server.
func StartServer() {
	templates = template.Must(template.ParseGlob("resources/*.templ.html"))

	mux := http.NewServeMux()
	mux.Handle("GET /static/", http.StripPrefix("/static", http.FileServer(filesOnly{http.Dir("resources/static")})))
	mux.HandleFunc("GET /{$}", index)
	mux.HandleFunc("GET /room/{roomid}", roomGET)
	mux.HandleFunc("POST /room-post/{roomid}", roomPOST)
	mux.HandleFunc("GET /stream/{roomid}", streamRoom)

	var handler http.Handler = rateLimit(recovery(mux))

	// Render sends SIGTERM before it stops an instance, e.g. on a deploy.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	var requestLog *requestLogger
	if databaseURL := os.Getenv("DATABASE_URL"); databaseURL != "" {
		var err error
		requestLog, err = newRequestLogger(ctx, databaseURL)
		if err != nil {
			log.Fatalf("request log: %v", err)
		}
		handler = requestLog.Middleware(handler)
		fmt.Println("Logging requests to the database")
	} else {
		fmt.Println("DATABASE_URL is not set, requests are not logged")
	}

	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}

	// Request contexts are cancelled when shutdown starts, so open event
	// streams end instead of keeping the shutdown waiting.
	requestCtx, cancelRequests := context.WithCancel(context.Background())
	server := &http.Server{
		Addr:              ":" + port,
		Handler:           handler,
		ReadHeaderTimeout: 10 * time.Second,
		BaseContext:       func(net.Listener) context.Context { return requestCtx },
	}
	server.RegisterOnShutdown(cancelRequests)

	serveErr := make(chan error, 1)
	go func() { serveErr <- server.ListenAndServe() }()

	select {
	case err := <-serveErr:
		log.Panicf("error: %s", err)
	case <-ctx.Done():
	}

	fmt.Println("Shutting down")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if err := server.Shutdown(shutdownCtx); err != nil && !errors.Is(err, context.DeadlineExceeded) {
		log.Printf("shutdown: %v", err)
	}
	if requestLog != nil {
		requestLog.Close(shutdownCtx)
	}
}

// filesOnly serves regular files and hides directories, so the file server
// never renders a directory listing.
type filesOnly struct {
	fs http.FileSystem
}

func (f filesOnly) Open(name string) (http.File, error) {
	file, err := f.fs.Open(name)
	if err != nil {
		return nil, err
	}
	if info, err := file.Stat(); err != nil || info.IsDir() {
		file.Close()
		return nil, fs.ErrNotExist
	}
	return file, nil
}
