package main

import (
	"fmt"
	"html/template"
	"io/fs"
	"log"
	"net/http"
	"os"
	"runtime"
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

	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}
	if err := http.ListenAndServe(":"+port, rateLimit(recovery(mux))); err != nil {
		log.Panicf("error: %s", err)
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
