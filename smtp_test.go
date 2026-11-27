package main

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"io"
	"math/big"
	"mime"
	"mime/quotedprintable"
	"net"
	"net/http"
	"net/mail"
	"net/textproto"
	"strings"
	"testing"
	"time"
)

func TestBuildMessage(t *testing.T) {
	m := winnerMail("anna@example.com", 42, []Prize{{Year: 2026, Month: 12, Day: 3, Description: "Gutschein über 50 € – sehr lange Beschreibung, die über die Zeilenlänge von quoted-printable hinausgeht und umbrochen werden muss", Sponsor: "Bäckerei Müller"}}, "https://example.org/abmelden?token=abc")
	raw := buildMessage(mail.Address{Name: "Adventskalender Würzburg", Address: "sender@example.org"}, m, time.Date(2026, 12, 3, 8, 0, 0, 0, time.UTC))

	for line := range strings.SplitSeq(string(raw), "\r\n") {
		if len(line) > 998 {
			t.Fatalf("line longer than 998 bytes: %q", line)
		}
	}
	msg, err := mail.ReadMessage(strings.NewReader(string(raw)))
	if err != nil {
		t.Fatal(err)
	}
	var dec mime.WordDecoder
	subject, err := dec.DecodeHeader(msg.Header.Get("Subject"))
	if err != nil || subject != m.Subject {
		t.Errorf("subject = %q, %v; want %q", subject, err, m.Subject)
	}
	from, err := msg.Header.AddressList("From")
	if err != nil || from[0].Name != "Adventskalender Würzburg" || from[0].Address != "sender@example.org" {
		t.Errorf("from = %v, %v", from, err)
	}
	if to := msg.Header.Get("To"); to != "<anna@example.com>" {
		t.Errorf("to = %q", to)
	}
	if got := msg.Header.Get("List-Unsubscribe"); got != "<https://example.org/abmelden?token=abc>" {
		t.Errorf("List-Unsubscribe = %q", got)
	}
	if got := msg.Header.Get("List-Unsubscribe-Post"); got != "List-Unsubscribe=One-Click" {
		t.Errorf("List-Unsubscribe-Post = %q", got)
	}
	if id := msg.Header.Get("Message-Id"); !strings.HasSuffix(id, "@example.org>") {
		t.Errorf("Message-Id = %q", id)
	}
	if _, err := msg.Header.Date(); err != nil {
		t.Errorf("Date: %v", err)
	}
	body, err := io.ReadAll(quotedprintable.NewReader(msg.Body))
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.ReplaceAll(string(body), "\r\n", "\n"); got != m.Text {
		t.Errorf("body = %q\nwant %q", got, m.Text)
	}
}

func TestSMTPMailer(t *testing.T) {
	for _, tc := range []struct {
		name          string
		implicitTLS   bool
		offerSTARTTLS bool
		wantErr       string
	}{
		{name: "STARTTLS", offerSTARTTLS: true},
		{name: "implicit TLS", implicitTLS: true},
		{name: "no STARTTLS", wantErr: "does not offer STARTTLS"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := startFakeSMTP(t, tc.implicitTLS, tc.offerSTARTTLS)
			host, port, _ := net.SplitHostPort(srv.addr)
			mailer := newSMTPMailer(host, port, "sender@example.org", "app-password", "sender@example.org", "Absender")
			mailer.implicitTLS = tc.implicitTLS
			mailer.tlsConfig = srv.clientTLS

			err := mailer.Send(t.Context(), Mail{To: "anna@example.com", Subject: "Grüße", Text: "Hallo,\n.\nZeile mit Punkt\n"})
			srv.wait()
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("err = %v, want %q", err, tc.wantErr)
				}
				if srv.auth != "" || srv.data != "" {
					t.Error("credentials or message sent without TLS")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if !srv.tls {
				t.Error("connection was not encrypted")
			}
			if srv.auth != "\x00sender@example.org\x00app-password" {
				t.Errorf("auth = %q", srv.auth)
			}
			if srv.from != "<sender@example.org>" || srv.rcpt != "<anna@example.com>" {
				t.Errorf("envelope = %q -> %q", srv.from, srv.rcpt)
			}
			msg, err := mail.ReadMessage(strings.NewReader(srv.data))
			if err != nil {
				t.Fatal(err)
			}
			body, _ := io.ReadAll(quotedprintable.NewReader(msg.Body))
			if got := strings.ReplaceAll(string(body), "\r\n", "\n"); got != "Hallo,\n.\nZeile mit Punkt\n" {
				t.Errorf("body = %q", got)
			}
		})
	}
}

func TestSMTPMailerTimeout(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() { // accepts, then never answers
		conn, err := ln.Accept()
		if err == nil {
			defer conn.Close()
			io.Copy(io.Discard, conn)
		}
	}()
	host, port, _ := net.SplitHostPort(ln.Addr().String())
	mailer := newSMTPMailer(host, port, "", "", "sender@example.org", "")
	ctx, cancel := context.WithTimeout(t.Context(), 200*time.Millisecond)
	defer cancel()
	start := time.Now()
	if err := mailer.Send(ctx, Mail{To: "anna@example.com"}); err == nil {
		t.Fatal("expected an error")
	}
	if d := time.Since(start); d > 5*time.Second {
		t.Fatalf("Send took %v despite the context deadline", d)
	}
}

func TestClientIP(t *testing.T) {
	for _, tc := range []struct {
		forwarded []string
		realIP    string
		want      string
	}{
		{want: "192.0.2.1"},
		{forwarded: []string{"203.0.113.7"}, want: "203.0.113.7"},
		{forwarded: []string{"6.6.6.6, 203.0.113.7"}, want: "203.0.113.7"},
		{forwarded: []string{"6.6.6.6", "203.0.113.7"}, want: "203.0.113.7"},
		{forwarded: []string{"2001:db8::1"}, want: "2001:db8::1"},
		{forwarded: []string{"garbage"}, want: "192.0.2.1"},
		{realIP: "6.6.6.6", want: "192.0.2.1"},
	} {
		r := &http.Request{RemoteAddr: "192.0.2.1:1234", Header: http.Header{}}
		for _, f := range tc.forwarded {
			r.Header.Add("X-Forwarded-For", f)
		}
		if tc.realIP != "" {
			r.Header.Set("X-Real-Ip", tc.realIP)
		}
		if got := clientIP(r); got != tc.want {
			t.Errorf("clientIP(%v, %q) = %q, want %q", tc.forwarded, tc.realIP, got, tc.want)
		}
	}
}

// fakeSMTP is a minimal SMTP server that records one message.
type fakeSMTP struct {
	addr      string
	clientTLS *tls.Config
	done      chan struct{}
	// recorded; read only after wait
	tls                    bool
	auth, from, rcpt, data string
}

func startFakeSMTP(t *testing.T, implicitTLS, offerSTARTTLS bool) *fakeSMTP {
	t.Helper()
	serverTLS, clientTLS := testTLSConfigs(t)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	s := &fakeSMTP{addr: ln.Addr().String(), clientTLS: clientTLS, done: make(chan struct{})}
	t.Cleanup(s.wait)
	go func() {
		defer close(s.done)
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		if implicitTLS {
			conn = tls.Server(conn, serverTLS)
			s.tls = true
		}
		defer func() { conn.Close() }()
		conn.SetDeadline(time.Now().Add(10 * time.Second))
		tp := textproto.NewConn(conn)
		tp.PrintfLine("220 fake ESMTP")
		for {
			line, err := tp.ReadLine()
			if err != nil {
				return
			}
			verb, arg, _ := strings.Cut(line, " ")
			switch strings.ToUpper(verb) {
			case "EHLO":
				if offerSTARTTLS && !s.tls {
					tp.PrintfLine("250-fake\r\n250 STARTTLS")
				} else {
					tp.PrintfLine("250-fake\r\n250 AUTH PLAIN")
				}
			case "STARTTLS":
				tp.PrintfLine("220 go ahead")
				conn = tls.Server(conn, serverTLS)
				tp = textproto.NewConn(conn)
				s.tls = true
			case "AUTH":
				_, b64, _ := strings.Cut(arg, " ")
				dec, _ := base64.StdEncoding.DecodeString(b64)
				s.auth = string(dec)
				tp.PrintfLine("235 ok")
			case "MAIL":
				s.from = strings.TrimPrefix(arg, "FROM:")
				if i := strings.IndexByte(s.from, ' '); i >= 0 {
					s.from = s.from[:i]
				}
				tp.PrintfLine("250 ok")
			case "RCPT":
				s.rcpt = strings.TrimPrefix(arg, "TO:")
				tp.PrintfLine("250 ok")
			case "DATA":
				tp.PrintfLine("354 go ahead")
				data, err := tp.ReadDotBytes()
				if err != nil {
					return
				}
				s.data = string(data)
				tp.PrintfLine("250 queued")
			case "QUIT":
				tp.PrintfLine("221 bye")
				return
			default:
				tp.PrintfLine("502 not implemented")
			}
		}
	}()
	return s
}

// wait blocks until the server has handled its connection.
func (s *fakeSMTP) wait() { <-s.done }

func testTLSConfigs(t *testing.T) (server, client *tls.Config) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		IPAddresses:  []net.IP{net.IPv4(127, 0, 0, 1)},
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	cert, _ := x509.ParseCertificate(der)
	pool := x509.NewCertPool()
	pool.AddCert(cert)
	server = &tls.Config{Certificates: []tls.Certificate{{Certificate: [][]byte{der}, PrivateKey: key}}}
	client = &tls.Config{ServerName: "127.0.0.1", RootCAs: pool}
	return server, client
}
