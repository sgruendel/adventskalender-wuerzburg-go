package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"embed"
	"encoding/base64"
	"errors"
	"html/template"
	"io/fs"
	"log"
	"net"
	"net/http"
	"net/mail"
	"runtime/debug"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

//go:embed web
var webFiles embed.FS

const (
	// confirmationValidFor is how long a confirmation link works. Unconfirmed
	// registrations are deleted after this time.
	confirmationValidFor = 72 * time.Hour
	// resendAfter is the minimum time between two confirmation emails for the
	// same registration, so the form cannot be used to flood a mailbox.
	resendAfter = 10 * time.Minute
	// registrationsPerHour limits registrations per client IP address.
	registrationsPerHour = 10
)

type server struct {
	db       *pgxpool.Pool
	mailer   Mailer
	baseURL  string
	pages    map[string]*template.Template
	static   http.Handler
	register *rateLimiter
}

func newServer(db *pgxpool.Pool, mailer Mailer, baseURL string) (*server, error) {
	s := &server{
		db:       db,
		mailer:   mailer,
		baseURL:  baseURL,
		pages:    map[string]*template.Template{},
		register: newRateLimiter(registrationsPerHour, time.Hour),
	}
	for _, name := range []string{"index.html", "message.html"} {
		t, err := template.ParseFS(webFiles, "web/templates/layout.html", "web/templates/"+name)
		if err != nil {
			return nil, err
		}
		s.pages[name] = t
	}
	static, err := fs.Sub(webFiles, "web/static")
	if err != nil {
		return nil, err
	}
	s.static = http.StripPrefix("/static", http.FileServer(filesOnly{http.FS(static)}))
	return s, nil
}

func (s *server) routes() http.Handler {
	mux := http.NewServeMux()
	mux.Handle("GET /static/", s.static)
	mux.HandleFunc("GET /{$}", s.handleIndex)
	mux.HandleFunc("POST /anmelden", s.handleRegister)
	mux.HandleFunc("GET /bestaetigen", s.handleConfirmPage)
	mux.HandleFunc("POST /bestaetigen", s.handleConfirm)
	mux.HandleFunc("GET /abmelden", s.handleUnsubscribePage)
	mux.HandleFunc("POST /abmelden", s.handleUnsubscribe)
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("ok"))
	})
	return mux
}

// Page data.

type indexPage struct {
	Error  string
	Email  string
	Number string
}

type messagePage struct {
	Title      string
	Paragraphs []string
	// Optional form with a single button that posts Token to Action.
	Action string
	Token  string
	Button string
}

// Handlers.

func (s *server) handleIndex(w http.ResponseWriter, r *http.Request) {
	s.render(w, http.StatusOK, "index.html", indexPage{})
}

func (s *server) handleRegister(w http.ResponseWriter, r *http.Request) {
	form := indexPage{
		Email:  strings.TrimSpace(r.PostFormValue("email")),
		Number: strings.TrimSpace(r.PostFormValue("kalender")),
	}
	if !s.register.allow(clientIP(r)) {
		form.Error = "Zu viele Anmeldungen von deinem Anschluss. Bitte versuch es später noch einmal."
		s.render(w, http.StatusTooManyRequests, "index.html", form)
		return
	}
	email, emailOK := normalizeEmail(form.Email)
	number, numberOK := parseCalendarNumber(form.Number)
	switch {
	case !emailOK:
		form.Error = "Bitte gib eine gültige E-Mail-Adresse ein."
	case !numberOK:
		form.Error = "Bitte gib die Kalendernummer ein, die oben links auf der Titelseite deines Kalenders steht (bis zu 4 Ziffern)."
	}
	if form.Error != "" {
		s.render(w, http.StatusBadRequest, "index.html", form)
		return
	}

	// Bots fill in the hidden "website" field. Pretend success to them.
	if r.PostFormValue("website") == "" {
		if err := s.startRegistration(r.Context(), email, number); err != nil {
			log.Printf("registration: %v", err)
			form.Error = "Die Bestätigungs-E-Mail konnte gerade nicht verschickt werden. Bitte versuch es in ein paar Minuten noch einmal."
			s.render(w, http.StatusServiceUnavailable, "index.html", form)
			return
		}
	}

	// The same answer whether or not the calendar was registered before,
	// so the form does not reveal who registered which calendar.
	s.render(w, http.StatusOK, "message.html", messagePage{
		Title: "Bitte bestätige deine Anmeldung",
		Paragraphs: []string{
			"Wir haben dir eine E-Mail an " + email + " geschickt. Bitte klick auf den Link darin, um die Benachrichtigung für Kalender " + formatNumber(number) + " zu aktivieren.",
			"Keine E-Mail bekommen? Schau bitte auch im Spam-Ordner nach. Falls du diesen Kalender schon bestätigt hast, ist nichts weiter zu tun.",
		},
	})
}

// startRegistration records a pending registration and sends the
// confirmation email. If the calendar is already confirmed, or a
// confirmation was sent recently, it does nothing.
func (s *server) startRegistration(ctx context.Context, email string, number int) error {
	token, tokenHash := newToken()
	send := false

	tx, err := s.db.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)

	var userID int64
	err = tx.QueryRow(ctx, `
		INSERT INTO users (email) VALUES ($1)
		ON CONFLICT (email) DO UPDATE SET email = EXCLUDED.email
		RETURNING id`, email).Scan(&userID)
	if err != nil {
		return err
	}

	var confirmed bool
	var sentAt time.Time
	err = tx.QueryRow(ctx, `
		SELECT confirmed_at IS NOT NULL, confirmation_sent_at
		FROM calendars WHERE user_id = $1 AND number = $2
		FOR UPDATE`, userID, number).Scan(&confirmed, &sentAt)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		unsubscribeToken, _ := newToken()
		tag, err := tx.Exec(ctx, `
			INSERT INTO calendars (user_id, number, confirm_token_hash, confirmation_sent_at, unsubscribe_token)
			VALUES ($1, $2, $3, now(), $4)
			ON CONFLICT (user_id, number) DO NOTHING`, userID, number, tokenHash, unsubscribeToken)
		if err != nil {
			return err
		}
		send = tag.RowsAffected() == 1 // 0 if a parallel request inserted it first
	case err != nil:
		return err
	case confirmed || time.Since(sentAt) < resendAfter:
		// Nothing to do.
	default:
		_, err := tx.Exec(ctx, `
			UPDATE calendars SET confirm_token_hash = $3, confirmation_sent_at = now()
			WHERE user_id = $1 AND number = $2`, userID, number, tokenHash)
		if err != nil {
			return err
		}
		send = true
	}

	if send {
		// Send before committing: if the email fails, nothing is stored and
		// the user can simply try again.
		mail := confirmationMail(email, number, s.baseURL+"/bestaetigen?token="+token)
		if err := s.mailer.Send(ctx, mail); err != nil {
			return err
		}
	}
	return tx.Commit(ctx)
}

func (s *server) handleConfirmPage(w http.ResponseWriter, r *http.Request) {
	token := r.URL.Query().Get("token")
	if token == "" {
		s.renderInvalidLink(w)
		return
	}
	s.render(w, http.StatusOK, "message.html", messagePage{
		Title:      "Anmeldung bestätigen",
		Paragraphs: []string{"Bitte bestätige mit einem Klick, dass du per E-Mail über Gewinne deines Adventskalenders benachrichtigt werden möchtest."},
		Action:     "/bestaetigen",
		Token:      token,
		Button:     "Anmeldung bestätigen",
	})
}

func (s *server) handleConfirm(w http.ResponseWriter, r *http.Request) {
	token := r.FormValue("token")
	var email, unsubscribeToken string
	var number int
	err := s.db.QueryRow(r.Context(), `
		UPDATE calendars c SET confirmed_at = now(), confirm_token_hash = NULL
		FROM users u
		WHERE u.id = c.user_id
		  AND c.confirm_token_hash = $1
		  AND c.confirmed_at IS NULL
		  AND c.confirmation_sent_at > now() - $2 * interval '1 second'
		RETURNING u.email, c.number, c.unsubscribe_token`,
		hashToken(token), confirmationValidFor.Seconds()).Scan(&email, &number, &unsubscribeToken)
	if errors.Is(err, pgx.ErrNoRows) {
		s.renderInvalidLink(w)
		return
	}
	if err != nil {
		s.serverError(w, err)
		return
	}

	// The welcome email carries the unsubscribe link. Without it the user
	// would have no way to unsubscribe until the first win.
	if err := s.mailer.Send(r.Context(), welcomeMail(email, number, s.unsubscribeURL(unsubscribeToken))); err != nil {
		log.Printf("welcome email for calendar %s: %v", formatNumber(number), err)
	}
	s.render(w, http.StatusOK, "message.html", messagePage{
		Title: "Anmeldung bestätigt",
		Paragraphs: []string{
			"Vielen Dank! Wir benachrichtigen dich per E-Mail, sobald Kalender " + formatNumber(number) + " gezogen wurde.",
			"Weitere Kalender kannst du auf der Startseite anmelden. Einen Link zum Abmelden findest du in jeder E-Mail von uns.",
		},
	})
}

func (s *server) handleUnsubscribePage(w http.ResponseWriter, r *http.Request) {
	token := r.URL.Query().Get("token")
	if token == "" {
		s.renderInvalidLink(w)
		return
	}
	s.render(w, http.StatusOK, "message.html", messagePage{
		Title:      "Abmelden",
		Paragraphs: []string{"Möchtest du keine Gewinnbenachrichtigungen mehr für diesen Kalender bekommen?"},
		Action:     "/abmelden",
		Token:      token,
		Button:     "Abmelden",
	})
}

// handleUnsubscribe serves both the form on the unsubscribe page and
// one-click unsubscribe requests from mail programs (RFC 8058), which post
// to the URL from the List-Unsubscribe header, token included.
func (s *server) handleUnsubscribe(w http.ResponseWriter, r *http.Request) {
	token := r.FormValue("token")
	number, err := s.unsubscribe(r.Context(), token)
	if errors.Is(err, pgx.ErrNoRows) {
		s.render(w, http.StatusOK, "message.html", messagePage{
			Title:      "Bereits abgemeldet",
			Paragraphs: []string{"Diese Anmeldung gibt es nicht mehr. Du bekommst keine weiteren E-Mails dazu."},
		})
		return
	}
	if err != nil {
		s.serverError(w, err)
		return
	}
	s.render(w, http.StatusOK, "message.html", messagePage{
		Title:      "Abgemeldet",
		Paragraphs: []string{"Du bekommst keine Benachrichtigungen mehr für Kalender " + formatNumber(number) + ". Deine Daten zu diesem Kalender sind gelöscht."},
	})
}

// unsubscribe deletes a calendar registration, and the user with it if it
// was their last one.
func (s *server) unsubscribe(ctx context.Context, token string) (number int, err error) {
	if token == "" {
		return 0, pgx.ErrNoRows
	}
	err = pgx.BeginFunc(ctx, s.db, func(tx pgx.Tx) error {
		var userID int64
		err := tx.QueryRow(ctx, `DELETE FROM calendars WHERE unsubscribe_token = $1 RETURNING user_id, number`,
			token).Scan(&userID, &number)
		if err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `
			DELETE FROM users u WHERE id = $1
			AND NOT EXISTS (SELECT 1 FROM calendars c WHERE c.user_id = u.id)`, userID)
		return err
	})
	return number, err
}

func (s *server) unsubscribeURL(token string) string {
	return s.baseURL + "/abmelden?token=" + token
}

func (s *server) renderInvalidLink(w http.ResponseWriter) {
	s.render(w, http.StatusNotFound, "message.html", messagePage{
		Title: "Link ungültig oder abgelaufen",
		Paragraphs: []string{
			"Dieser Link funktioniert nicht mehr. Bestätigungslinks sind " + strconv.Itoa(int(confirmationValidFor.Hours()/24)) + " Tage gültig und können nur einmal verwendet werden.",
			"Falls du deine Anmeldung schon bestätigt hast, ist nichts weiter zu tun. Sonst melde deinen Kalender auf der Startseite einfach noch einmal an.",
		},
	})
}

func (s *server) serverError(w http.ResponseWriter, err error) {
	log.Printf("error: %v", err)
	s.render(w, http.StatusInternalServerError, "message.html", messagePage{
		Title:      "Etwas ist schiefgelaufen",
		Paragraphs: []string{"Bitte versuch es in ein paar Minuten noch einmal."},
	})
}

// render renders a page completely before writing it, so a template error
// results in a clean 500 instead of a half-written page.
func (s *server) render(w http.ResponseWriter, status int, name string, data any) {
	var buf bytes.Buffer
	if err := s.pages[name].ExecuteTemplate(&buf, "layout", data); err != nil {
		log.Printf("rendering %s: %v", name, err)
		http.Error(w, http.StatusText(http.StatusInternalServerError), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	w.Write(buf.Bytes())
}

// Input validation.

// normalizeEmail checks that s is a plain email address (no display name)
// and returns it in lower case.
func normalizeEmail(s string) (string, bool) {
	s = strings.TrimSpace(s)
	if s == "" || len(s) > 254 {
		return "", false
	}
	addr, err := mail.ParseAddress(s)
	if err != nil || addr.Address != s {
		return "", false
	}
	at := strings.LastIndexByte(s, '@')
	domain := s[at+1:]
	if at < 1 || !strings.Contains(domain, ".") || strings.HasPrefix(domain, ".") || strings.HasSuffix(domain, ".") {
		return "", false
	}
	return strings.ToLower(s), true
}

// parseCalendarNumber accepts 1 to 4 digits, with or without leading zeros.
func parseCalendarNumber(s string) (int, bool) {
	s = strings.TrimSpace(s)
	if len(s) < 1 || len(s) > 4 {
		return 0, false
	}
	for _, c := range s {
		if c < '0' || c > '9' {
			return 0, false
		}
	}
	n, err := strconv.Atoi(s)
	return n, err == nil
}

// Tokens.

// newToken returns a random URL-safe token and its hash. Only the hash of
// confirmation tokens is stored.
func newToken() (token string, hash []byte) {
	b := make([]byte, 32)
	rand.Read(b)
	token = base64.RawURLEncoding.EncodeToString(b)
	return token, hashToken(token)
}

func hashToken(token string) []byte {
	h := sha256.Sum256([]byte(token))
	return h[:]
}

// Middleware and helpers.

// securityHeaders forbids external resources, framing and referrers. The
// referrer matters because confirmation and unsubscribe URLs contain tokens.
func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Content-Security-Policy", "default-src 'none'; style-src 'self'; img-src 'self' data:; form-action 'self'; base-uri 'none'; frame-ancestors 'none'")
		h.Set("Referrer-Policy", "no-referrer")
		h.Set("X-Content-Type-Options", "nosniff")
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

// clientIP returns the client address. Behind a reverse proxy it is the
// last X-Forwarded-For entry: the one the proxy itself appended. Earlier
// entries and other headers such as X-Real-Ip come from the client and can
// be forged. The app must therefore only be reachable through the proxy.
func clientIP(r *http.Request) string {
	if forwarded := r.Header.Values("X-Forwarded-For"); len(forwarded) > 0 {
		last := forwarded[len(forwarded)-1]
		if i := strings.LastIndexByte(last, ','); i >= 0 {
			last = last[i+1:]
		}
		if ip := strings.TrimSpace(last); net.ParseIP(ip) != nil {
			return ip
		}
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// rateLimiter allows up to limit events per key in each window.
type rateLimiter struct {
	mu      sync.Mutex
	limit   int
	window  time.Duration
	entries map[string]*rateWindow
}

type rateWindow struct {
	start time.Time
	count int
}

func newRateLimiter(limit int, window time.Duration) *rateLimiter {
	return &rateLimiter{limit: limit, window: window, entries: map[string]*rateWindow{}}
}

func (l *rateLimiter) allow(key string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := time.Now()
	if len(l.entries) > 10_000 {
		for k, e := range l.entries {
			if now.Sub(e.start) > l.window {
				delete(l.entries, k)
			}
		}
	}
	e, ok := l.entries[key]
	if !ok || now.Sub(e.start) > l.window {
		e = &rateWindow{start: now}
		l.entries[key] = e
	}
	e.count++
	return e.count <= l.limit
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
