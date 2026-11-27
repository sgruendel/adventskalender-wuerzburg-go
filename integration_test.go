package main

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

// The integration tests need an empty Postgres database they may wipe:
//
//	TEST_DATABASE_URL=postgres://... go test ./...

type recordingMailer struct {
	mu    sync.Mutex
	mails []Mail
	err   error
}

func (m *recordingMailer) Send(_ context.Context, mail Mail) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.err != nil {
		return m.err
	}
	m.mails = append(m.mails, mail)
	return nil
}

func (m *recordingMailer) take() []Mail {
	m.mu.Lock()
	defer m.mu.Unlock()
	mails := m.mails
	m.mails = nil
	return mails
}

func testDB(t *testing.T) *pgxpool.Pool {
	t.Helper()
	databaseURL := os.Getenv("TEST_DATABASE_URL")
	if databaseURL == "" {
		t.Skip("TEST_DATABASE_URL is not set")
	}
	ctx := context.Background()
	db, err := openDB(ctx, databaseURL, 4)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(db.Close)
	_, err = db.Exec(ctx, `TRUNCATE users, calendars, prizes, notifications, request_log RESTART IDENTITY CASCADE`)
	if err != nil {
		t.Fatal(err)
	}
	return db
}

var linkPattern = regexp.MustCompile(`https?://\S+`)

func linkIn(t *testing.T, m Mail, path string) string {
	t.Helper()
	for _, link := range linkPattern.FindAllString(m.Text, -1) {
		if strings.Contains(link, path) {
			return link
		}
	}
	t.Fatalf("no %s link in mail %q:\n%s", path, m.Subject, m.Text)
	return ""
}

type testClient struct {
	t      *testing.T
	client *http.Client
	base   string
}

func (c testClient) do(method, path string, form url.Values) (int, string) {
	c.t.Helper()
	var body io.Reader
	if form != nil {
		body = strings.NewReader(form.Encode())
	}
	req, err := http.NewRequest(method, c.base+path, body)
	if err != nil {
		c.t.Fatal(err)
	}
	if form != nil {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	resp, err := c.client.Do(req)
	if err != nil {
		c.t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

// pathOf strips the base URL from a link in an email.
func pathOf(t *testing.T, link string) string {
	t.Helper()
	u, err := url.Parse(link)
	if err != nil {
		t.Fatal(err)
	}
	return u.RequestURI()
}

func TestRegistrationAndNotificationFlow(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	mailer := &recordingMailer{}

	ts := httptest.NewUnstartedServer(nil)
	app, err := newServer(db, mailer, "http://"+ts.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	ts.Config.Handler = securityHeaders(recovery(app.routes()))
	ts.Start()
	defer ts.Close()
	jar, _ := cookiejar.New(nil)
	c := testClient{t: t, client: &http.Client{Jar: jar}, base: ts.URL}

	// Invalid input is rejected and nothing is sent.
	status, body := c.do("POST", "/anmelden", url.Values{"email": {"kein-email"}, "kalender": {"1130"}})
	if status != http.StatusBadRequest || !strings.Contains(body, "gültige E-Mail-Adresse") {
		t.Fatalf("invalid email: status %d", status)
	}
	status, _ = c.do("POST", "/anmelden", url.Values{"email": {"max@example.de"}, "kalender": {"12345"}})
	if status != http.StatusBadRequest {
		t.Fatalf("invalid number: status %d", status)
	}
	if n := len(mailer.take()); n != 0 {
		t.Fatalf("%d mails sent for invalid input", n)
	}

	// Register two calendars for the same address.
	for _, number := range []string{"1130", "0640"} {
		status, body = c.do("POST", "/anmelden", url.Values{"email": {"Max@Example.de"}, "kalender": {number}})
		if status != http.StatusOK || !strings.Contains(body, "Bitte bestätige deine Anmeldung") {
			t.Fatalf("register %s: status %d\n%s", number, status, body)
		}
	}
	mails := mailer.take()
	if len(mails) != 2 || mails[0].To != "max@example.de" || !strings.Contains(mails[0].Subject, "1130") {
		t.Fatalf("confirmation mails = %+v", mails)
	}
	confirm1130 := linkIn(t, mails[0], "/bestaetigen?token=")

	// Registering again right away does not send another email.
	c.do("POST", "/anmelden", url.Values{"email": {"max@example.de"}, "kalender": {"1130"}})
	if n := len(mailer.take()); n != 0 {
		t.Fatalf("resent confirmation within %v: %d mails", resendAfter, n)
	}

	// The bot trap: looks successful, sends nothing, stores nothing.
	status, _ = c.do("POST", "/anmelden", url.Values{"email": {"bot@example.de"}, "kalender": {"1"}, "website": {"x"}})
	if status != http.StatusOK || len(mailer.take()) != 0 {
		t.Fatalf("honeypot: status %d", status)
	}

	// Opening the link only shows a button; it does not confirm by itself.
	status, body = c.do("GET", pathOf(t, confirm1130), nil)
	if status != http.StatusOK || !strings.Contains(body, `action="/bestaetigen"`) {
		t.Fatalf("confirm page: status %d", status)
	}
	var confirmed int
	db.QueryRow(ctx, `SELECT count(*) FROM calendars WHERE confirmed_at IS NOT NULL`).Scan(&confirmed)
	if confirmed != 0 {
		t.Fatal("GET confirmed the registration")
	}

	// Confirm 1130 only; 0640 stays unconfirmed.
	token := strings.TrimPrefix(pathOf(t, confirm1130), "/bestaetigen?token=")
	status, body = c.do("POST", "/bestaetigen", url.Values{"token": {token}})
	if status != http.StatusOK || !strings.Contains(body, "Anmeldung bestätigt") {
		t.Fatalf("confirm: status %d\n%s", status, body)
	}
	mails = mailer.take()
	if len(mails) != 1 || !strings.Contains(mails[0].Subject, "bestätigt") || mails[0].UnsubscribeURL == "" {
		t.Fatalf("welcome mails = %+v", mails)
	}
	unsubscribe1130 := mails[0].UnsubscribeURL

	// A confirmation link works only once.
	status, _ = c.do("POST", "/bestaetigen", url.Values{"token": {token}})
	if status != http.StatusNotFound {
		t.Fatalf("second confirm: status %d", status)
	}

	// The daily check: 1130 won twice, 0640 won but is unconfirmed.
	prizes := []Prize{
		{ID: 1, Year: 2025, Month: 12, Day: 1, CalendarNumber: 1130, Description: "Taschenmesser", Sponsor: "Optik Horn"},
		{ID: 2, Year: 2025, Month: 12, Day: 3, CalendarNumber: 640, Description: "Gutschein", Sponsor: "Bäckerei"},
		{ID: 3, Year: 2025, Month: 12, Day: 5, CalendarNumber: 2435, Description: "Zahnbürste", Sponsor: "Praxis"},
	}
	chk := &checker{
		db:      db,
		mailer:  mailer,
		baseURL: app.baseURL,
		fetchPrizes: func(ctx context.Context, year int) ([]Prize, error) {
			return prizes, nil
		},
	}
	if err := chk.run(ctx, 2025, false, 100); err != nil {
		t.Fatal(err)
	}
	mails = mailer.take()
	if len(mails) != 1 || mails[0].To != "max@example.de" ||
		!strings.Contains(mails[0].Text, "01.12.2025: Taschenmesser") ||
		mails[0].UnsubscribeURL != unsubscribe1130 {
		t.Fatalf("winner mails = %+v", mails)
	}

	// Running again sends nothing; a new prize for 1130 is sent on its own.
	if err := chk.run(ctx, 2025, false, 100); err != nil {
		t.Fatal(err)
	}
	if n := len(mailer.take()); n != 0 {
		t.Fatalf("second run sent %d mails", n)
	}
	prizes = append(prizes, Prize{ID: 4, Year: 2025, Month: 12, Day: 7, CalendarNumber: 1130, Description: "Buch", Sponsor: "Buchhandlung"})
	if err := chk.run(ctx, 2025, false, 100); err != nil {
		t.Fatal(err)
	}
	mails = mailer.take()
	if len(mails) != 1 || !strings.Contains(mails[0].Text, "07.12.2025: Buch") || strings.Contains(mails[0].Text, "Taschenmesser") {
		t.Fatalf("mail for new prize = %+v", mails)
	}

	// A failed email is retried on the next run.
	prizes = append(prizes, Prize{ID: 5, Year: 2025, Month: 12, Day: 9, CalendarNumber: 1130, Description: "Kerze", Sponsor: "Laden"})
	mailer.err = errors.New("Brevo down")
	if err := chk.run(ctx, 2025, false, 100); err == nil {
		t.Fatal("run with failing mailer returned no error")
	}
	mailer.err = nil
	if err := chk.run(ctx, 2025, false, 100); err != nil {
		t.Fatal(err)
	}
	if mails = mailer.take(); len(mails) != 1 || !strings.Contains(mails[0].Text, "Kerze") {
		t.Fatalf("retry mails = %+v", mails)
	}

	// The safety limit stops a run with too many emails.
	prizes = append(prizes, Prize{ID: 6, Year: 2025, Month: 12, Day: 11, CalendarNumber: 1130, Description: "Tee", Sponsor: "Laden"})
	if err := chk.run(ctx, 2025, false, 0); err == nil || len(mailer.take()) != 0 {
		t.Fatal("max-mails limit did not stop the run")
	}

	// One-click unsubscribe, as a mail program sends it.
	status, body = c.do("POST", pathOf(t, unsubscribe1130), url.Values{"List-Unsubscribe": {"One-Click"}})
	if status != http.StatusOK || !strings.Contains(body, "Abgemeldet") {
		t.Fatalf("unsubscribe: status %d\n%s", status, body)
	}
	status, body = c.do("POST", pathOf(t, unsubscribe1130), url.Values{"List-Unsubscribe": {"One-Click"}})
	if status != http.StatusOK || !strings.Contains(body, "Bereits abgemeldet") {
		t.Fatalf("second unsubscribe: status %d", status)
	}

	// The user still exists because 0640 is pending; purging removes it
	// once the confirmation has expired, and the user with it.
	db.Exec(ctx, `UPDATE calendars SET confirmation_sent_at = now() - interval '4 days'`)
	if err := purgeUnconfirmed(ctx, db); err != nil {
		t.Fatal(err)
	}
	db.Exec(ctx, `UPDATE users SET created_at = now() - interval '2 hours'`)
	if err := purgeUnconfirmed(ctx, db); err != nil {
		t.Fatal(err)
	}
	var users, calendars int
	db.QueryRow(ctx, `SELECT (SELECT count(*) FROM users), (SELECT count(*) FROM calendars)`).Scan(&users, &calendars)
	if users != 0 || calendars != 0 {
		t.Fatalf("after unsubscribe and purge: %d users, %d calendars", users, calendars)
	}
}

func TestRegistrationRollsBackWhenMailFails(t *testing.T) {
	db := testDB(t)
	mailer := &recordingMailer{err: errors.New("Brevo down")}
	app, err := newServer(db, mailer, "http://example.invalid")
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(app.routes())
	defer ts.Close()
	c := testClient{t: t, client: ts.Client(), base: ts.URL}

	status, body := c.do("POST", "/anmelden", url.Values{"email": {"max@example.de"}, "kalender": {"42"}})
	if status != http.StatusServiceUnavailable || !strings.Contains(body, "nicht verschickt") {
		t.Fatalf("status %d\n%s", status, body)
	}
	// Nothing stored, so a retry sends the email right away.
	mailer.err = nil
	c.do("POST", "/anmelden", url.Values{"email": {"max@example.de"}, "kalender": {"42"}})
	if n := len(mailer.take()); n != 1 {
		t.Fatalf("retry sent %d mails, want 1", n)
	}
}

func TestRegistrationRateLimit(t *testing.T) {
	db := testDB(t)
	mailer := &recordingMailer{}
	app, err := newServer(db, mailer, "http://example.invalid")
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(app.routes())
	defer ts.Close()
	c := testClient{t: t, client: ts.Client(), base: ts.URL}

	for i := range registrationsPerHour + 1 {
		status, _ := c.do("POST", "/anmelden", url.Values{"email": {"max@example.de"}, "kalender": {strconv.Itoa(i + 1)}})
		want := http.StatusOK
		if i == registrationsPerHour {
			want = http.StatusTooManyRequests
		}
		if status != want {
			t.Fatalf("registration %d: status %d, want %d", i+1, status, want)
		}
	}
}
