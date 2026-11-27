package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/tls"
	"encoding/hex"
	"errors"
	"fmt"
	"mime"
	"mime/quotedprintable"
	"net"
	"net/mail"
	"net/smtp"
	"strings"
	"time"
)

// smtpMailer sends email through an SMTP server that requires TLS, e.g.
// smtp.gmail.com with an app password. Port 465 uses implicit TLS, every
// other port STARTTLS.
type smtpMailer struct {
	host, port         string
	username, password string
	from               mail.Address
	implicitTLS        bool
	tlsConfig          *tls.Config // nil: verify against the system roots
}

func newSMTPMailer(host, port, username, password, fromEmail, fromName string) *smtpMailer {
	return &smtpMailer{
		host:        host,
		port:        port,
		username:    username,
		password:    password,
		from:        mail.Address{Name: fromName, Address: fromEmail},
		implicitTLS: port == "465",
	}
}

func (s *smtpMailer) Send(ctx context.Context, m Mail) error {
	if err := s.send(ctx, m); err != nil {
		return fmt.Errorf("sending email via %s: %w", s.host, err)
	}
	return nil
}

func (s *smtpMailer) send(ctx context.Context, m Mail) error {
	msg := buildMessage(s.from, m, time.Now())

	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	var dialer net.Dialer
	conn, err := dialer.DialContext(ctx, "tcp", net.JoinHostPort(s.host, s.port))
	if err != nil {
		return err
	}
	// net/smtp has no context support: end blocked reads and writes through
	// the connection deadline instead.
	deadline, _ := ctx.Deadline()
	conn.SetDeadline(deadline)
	stop := context.AfterFunc(ctx, func() { conn.SetDeadline(time.Now()) })
	defer stop()

	tlsConfig := s.tlsConfig
	if tlsConfig == nil {
		tlsConfig = &tls.Config{ServerName: s.host}
	}
	if s.implicitTLS {
		conn = tls.Client(conn, tlsConfig)
	}
	c, err := smtp.NewClient(conn, s.host)
	if err != nil {
		conn.Close()
		return err
	}
	defer c.Close()

	if !s.implicitTLS {
		// Never send the password or addresses unencrypted.
		if ok, _ := c.Extension("STARTTLS"); !ok {
			return errors.New("server does not offer STARTTLS")
		}
		if err := c.StartTLS(tlsConfig); err != nil {
			return err
		}
	}
	if s.username != "" {
		if err := c.Auth(smtp.PlainAuth("", s.username, s.password, s.host)); err != nil {
			return err
		}
	}
	if err := c.Mail(s.from.Address); err != nil {
		return err
	}
	if err := c.Rcpt(m.To); err != nil {
		return err
	}
	w, err := c.Data()
	if err != nil {
		return err
	}
	if _, err := w.Write(msg); err != nil {
		return err
	}
	if err := w.Close(); err != nil {
		return err
	}
	return c.Quit()
}

// buildMessage formats m as a UTF-8 plain-text email (RFC 5322) with
// quoted-printable body.
func buildMessage(from mail.Address, m Mail, now time.Time) []byte {
	var b bytes.Buffer
	header := func(name, value string) {
		b.WriteString(name + ": " + value + "\r\n")
	}
	header("From", from.String())
	header("To", (&mail.Address{Address: m.To}).String())
	header("Subject", mime.QEncoding.Encode("utf-8", m.Subject))
	header("Date", now.Format(time.RFC1123Z))
	header("Message-ID", messageID(from.Address))
	header("MIME-Version", "1.0")
	header("Content-Type", "text/plain; charset=utf-8")
	header("Content-Transfer-Encoding", "quoted-printable")
	if m.UnsubscribeURL != "" {
		header("List-Unsubscribe", "<"+m.UnsubscribeURL+">")
		header("List-Unsubscribe-Post", "List-Unsubscribe=One-Click")
	}
	b.WriteString("\r\n")

	qp := quotedprintable.NewWriter(&b)
	qp.Write([]byte(strings.ReplaceAll(m.Text, "\n", "\r\n")))
	qp.Close()
	return b.Bytes()
}

func messageID(fromAddress string) string {
	_, domain, ok := strings.Cut(fromAddress, "@")
	if !ok {
		domain = "localhost"
	}
	id := make([]byte, 16)
	rand.Read(id)
	return "<" + hex.EncodeToString(id) + "@" + domain + ">"
}
