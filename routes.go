package main

import (
	"bytes"
	"encoding/json/v2"
	"fmt"
	"html"
	"html/template"
	"io"
	"log"
	"net"
	"net/http"
	"runtime/debug"
	"strings"
	"time"
)

var templates *template.Template

// jsonOptions keeps map keys sorted, so responses are stable byte for byte.
var jsonOptions = json.Deterministic(true)

// rateLimit blocks a client once it has made 200 requests.
func rateLimit(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ip := clientIP(r)
		value := int(ips.Add(ip, 1))
		if value%50 == 0 {
			fmt.Printf("ip: %s, count: %d\n", ip, value)
		}
		if value >= 200 {
			if value%200 == 0 {
				fmt.Println("ip blocked")
			}
			w.Header().Set("Content-Type", "text/plain; charset=utf-8")
			w.WriteHeader(http.StatusServiceUnavailable)
			io.WriteString(w, "you were automatically banned :)")
			return
		}
		next.ServeHTTP(w, r)
	})
}

// recovery turns a panic in a handler into a 500 response.
func recovery(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if err := recover(); err != nil {
				if err == http.ErrAbortHandler {
					panic(err)
				}
				log.Printf("panic serving %s %s: %v\n%s", r.Method, r.URL.Path, err, debug.Stack())
				w.WriteHeader(http.StatusInternalServerError)
			}
		}()
		next.ServeHTTP(w, r)
	})
}

// clientIP returns the client address, preferring the X-Forwarded-For and
// X-Real-Ip headers set by a reverse proxy.
func clientIP(r *http.Request) string {
	if forwarded := r.Header.Get("X-Forwarded-For"); forwarded != "" {
		first, _, _ := strings.Cut(forwarded, ",")
		if ip := strings.TrimSpace(first); net.ParseIP(ip) != nil {
			return ip
		}
	}
	if ip := strings.TrimSpace(r.Header.Get("X-Real-Ip")); net.ParseIP(ip) != nil {
		return ip
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

func index(w http.ResponseWriter, r *http.Request) {
	http.Redirect(w, r, "/room/hn", http.StatusMovedPermanently)
}

func roomGET(w http.ResponseWriter, r *http.Request) {
	roomid := r.PathValue("roomid")
	nick := r.URL.Query().Get("nick")
	if len(nick) < 2 {
		nick = ""
	}
	if len(nick) > 13 {
		nick = nick[0:12] + "..."
	}
	renderHTML(w, http.StatusOK, "room_login.templ.html", map[string]any{
		"roomid":    roomid,
		"nick":      nick,
		"timestamp": time.Now().Unix(),
	})
}

func roomPOST(w http.ResponseWriter, r *http.Request) {
	roomid := r.PathValue("roomid")
	// JSON v2 refuses to encode invalid UTF-8, so replace it here. Otherwise a
	// broadcast message would break the event stream of every listener.
	nick := strings.ToValidUTF8(r.URL.Query().Get("nick"), "\uFFFD")
	message := strings.ToValidUTF8(r.PostFormValue("message"), "\uFFFD")
	message = strings.TrimSpace(message)

	validMessage := len(message) > 1 && len(message) < 200
	validNick := len(nick) > 1 && len(nick) < 14
	if !validMessage || !validNick {
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"status": "failed",
			"error":  "the message or nickname is too long",
		})
		return
	}

	post := map[string]string{
		"nick":    html.EscapeString(nick),
		"message": html.EscapeString(message),
	}
	messages.Add("inbound", 1)
	room(roomid).Submit(post)
	writeJSON(w, http.StatusOK, post)
}

func streamRoom(w http.ResponseWriter, r *http.Request) {
	roomid := r.PathValue("roomid")
	listener := openListener(roomid)
	ticker := time.NewTicker(1 * time.Second)
	users.Add("connected", 1)
	defer func() {
		closeListener(roomid, listener)
		ticker.Stop()
		users.Add("disconnected", 1)
	}()

	rc := http.NewResponseController(w)
	w.Header().Set("Content-Type", "text/event-stream;charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(http.StatusOK)
	if err := rc.Flush(); err != nil {
		return
	}

	for {
		var err error
		select {
		case <-r.Context().Done():
			return
		case msg := <-listener:
			messages.Add("outbound", 1)
			err = writeSSE(w, "message", msg)
		case <-ticker.C:
			// Stats are nil until statsWorker has completed its first run.
			if stats := Stats(); stats != nil {
				err = writeSSE(w, "stats", stats)
			}
		}
		if err == nil {
			err = rc.Flush()
		}
		if err != nil {
			return
		}
	}
}

// renderHTML renders a template completely before writing it, so a template
// error results in a clean 500 instead of a half-written page.
func renderHTML(w http.ResponseWriter, status int, name string, data any) {
	var buf bytes.Buffer
	if err := templates.ExecuteTemplate(&buf, name, data); err != nil {
		log.Printf("rendering %s: %v", name, err)
		http.Error(w, http.StatusText(http.StatusInternalServerError), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	w.Write(buf.Bytes())
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	body, err := json.Marshal(v, jsonOptions)
	if err != nil {
		log.Printf("encoding JSON: %v", err)
		http.Error(w, http.StatusText(http.StatusInternalServerError), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	w.Write(body)
}

// writeSSE writes one server-sent event with a JSON-encoded payload.
func writeSSE(w io.Writer, event string, data any) error {
	payload, err := json.Marshal(data, jsonOptions)
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(w, "event:%s\ndata:%s\n\n", event, payload)
	return err
}
