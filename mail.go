package main

import (
	"bytes"
	"context"
	"encoding/json/v2"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"time"
)

// Mail is a plain-text email to one recipient.
type Mail struct {
	To      string
	Subject string
	Text    string
	// UnsubscribeURL, if set, is sent in the List-Unsubscribe headers, so
	// mail programs can offer a one-click unsubscribe (RFC 8058).
	UnsubscribeURL string
}

type Mailer interface {
	Send(ctx context.Context, m Mail) error
}

const brevoAPIURL = "https://api.brevo.com/v3/smtp/email"

// brevoMailer sends email through Brevo's transactional email API.
type brevoMailer struct {
	url       string
	apiKey    string
	fromEmail string
	fromName  string
	client    *http.Client
}

func newBrevoMailer(apiKey, fromEmail, fromName string) *brevoMailer {
	return &brevoMailer{
		url:       brevoAPIURL,
		apiKey:    apiKey,
		fromEmail: fromEmail,
		fromName:  fromName,
		client:    &http.Client{Timeout: 20 * time.Second},
	}
}

type brevoAddress struct {
	Email string `json:"email"`
	Name  string `json:"name,omitempty"`
}

type brevoRequest struct {
	Sender      brevoAddress      `json:"sender"`
	To          []brevoAddress    `json:"to"`
	Subject     string            `json:"subject"`
	TextContent string            `json:"textContent"`
	Headers     map[string]string `json:"headers,omitempty"`
}

func (b *brevoMailer) Send(ctx context.Context, m Mail) error {
	req := brevoRequest{
		Sender:      brevoAddress{Email: b.fromEmail, Name: b.fromName},
		To:          []brevoAddress{{Email: m.To}},
		Subject:     m.Subject,
		TextContent: m.Text,
	}
	if m.UnsubscribeURL != "" {
		req.Headers = map[string]string{
			"List-Unsubscribe":      "<" + m.UnsubscribeURL + ">",
			"List-Unsubscribe-Post": "List-Unsubscribe=One-Click",
		}
	}
	body, err := json.Marshal(req)
	if err != nil {
		return err
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, b.url, bytes.NewReader(body))
	if err != nil {
		return err
	}
	httpReq.Header.Set("api-key", b.apiKey)
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Accept", "application/json")

	resp, err := b.client.Do(httpReq)
	if err != nil {
		return fmt.Errorf("sending email via Brevo: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return fmt.Errorf("sending email via Brevo: %s: %s", resp.Status, strings.TrimSpace(string(msg)))
	}
	return nil
}

// logMailer prints emails instead of sending them, for local development.
type logMailer struct{}

func (logMailer) Send(_ context.Context, m Mail) error {
	log.Printf("email (not sent)\nTo: %s\nSubject: %s\n\n%s\n", m.To, m.Subject, m.Text)
	return nil
}

// Email texts.

const officialListURL = "https://www.adventskalender-wuerzburg.de/gewinne"

const mailFooter = `
--
Dies ist ein privater Benachrichtigungsdienst ohne Gewähr und kein Angebot
der Lions Clubs Würzburg. Maßgeblich ist die offizielle Gewinnliste:
` + officialListURL + "\n"

func formatNumber(n int) string {
	return fmt.Sprintf("%04d", n)
}

func confirmationMail(to string, number int, confirmURL string) Mail {
	return Mail{
		To:      to,
		Subject: "Bitte bestätigen: Gewinnbenachrichtigung für Kalender " + formatNumber(number),
		Text: fmt.Sprintf(`Hallo,

du möchtest per E-Mail benachrichtigt werden, wenn die Nummer deines
Würzburger Lions-Adventskalenders gezogen wird:

    Kalendernummer: %s

Bitte bestätige die Anmeldung über diesen Link:

    %s

Der Link ist %d Tage gültig. Falls du dich nicht angemeldet hast,
ignoriere diese E-Mail bitte. Deine Adresse wird dann automatisch
gelöscht.
%s`, formatNumber(number), confirmURL, int(confirmationValidFor.Hours()/24), mailFooter),
	}
}

func welcomeMail(to string, number int, unsubscribeURL string) Mail {
	return Mail{
		To:             to,
		Subject:        "Anmeldung bestätigt: Kalender " + formatNumber(number),
		UnsubscribeURL: unsubscribeURL,
		Text: fmt.Sprintf(`Hallo,

deine Anmeldung ist bestätigt. Vom 1. bis 24. Dezember prüfen wir täglich
die veröffentlichten Gewinnnummern und schicken dir eine E-Mail, sobald
Kalender %s gezogen wurde. Gewinne, die schon vor deiner Anmeldung gezogen
wurden, melden wir dir ebenfalls.

Wenn du keine Benachrichtigungen mehr möchtest, kannst du dich hier
abmelden:

    %s
%s`, formatNumber(number), unsubscribeURL, mailFooter),
	}
}

func winnerMail(to string, number int, prizes []Prize, unsubscribeURL string) Mail {
	var list strings.Builder
	for _, p := range prizes {
		fmt.Fprintf(&list, "    %02d.%02d.%d: %s\n", p.Day, p.Month, p.Year, p.Description)
		if p.Sponsor != "" {
			fmt.Fprintf(&list, "                Sponsor: %s\n", p.Sponsor)
		}
	}
	subject := "Gewonnen! Dein Adventskalender " + formatNumber(number) + " wurde gezogen"
	return Mail{
		To:             to,
		Subject:        subject,
		UnsubscribeURL: unsubscribeURL,
		Text: fmt.Sprintf(`Hallo,

herzlichen Glückwunsch! Die Nummer deines Adventskalenders, %s, steht in
der Gewinnliste:

%s
Laut Veranstalter können die Gewinne bei den jeweiligen Sponsoren unter
Vorlage des Kalenders abgeholt werden. Bitte prüfe deinen Gewinn vorher
in der offiziellen Gewinnliste:

    %s

Viel Freude mit deinem Gewinn!

Du bekommst diese E-Mail, weil du Kalender %s für die
Gewinnbenachrichtigung angemeldet hast. Abmelden:

    %s
%s`, formatNumber(number), list.String(), officialListURL, formatNumber(number), unsubscribeURL, mailFooter),
	}
}
