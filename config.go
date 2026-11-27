package main

import (
	"errors"
	"log"
	"os"
	"strings"
)

// config is read from environment variables:
//
//	DATABASE_URL     Postgres connection string (required)
//	BASE_URL         public URL of the web service, used for links in emails;
//	                 without it, the app runs in development mode
//	                 (http://localhost:PORT, emails only printed to the log)
//	PORT             port of the web service (default 8080)
//	SMTP_HOST        SMTP server, e.g. smtp.gmail.com
//	SMTP_PORT        default 587 (STARTTLS); 465 uses implicit TLS
//	SMTP_USERNAME    SMTP login, e.g. the Gmail address
//	SMTP_PASSWORD    SMTP password, for Gmail an app password
//	BREVO_API_KEY    alternative to SMTP: send through the Brevo API
//	MAIL_FROM_EMAIL  sender address (default: SMTP_USERNAME)
//	MAIL_FROM_NAME   sender name (optional)
type config struct {
	databaseURL   string
	baseURL       string
	port          string
	devMode       bool
	smtpHost      string
	smtpPort      string
	smtpUsername  string
	smtpPassword  string
	brevoAPIKey   string
	mailFromEmail string
	mailFromName  string
}

func loadConfig() (config, error) {
	cfg := config{
		databaseURL:   os.Getenv("DATABASE_URL"),
		baseURL:       os.Getenv("BASE_URL"),
		port:          firstNonEmpty(os.Getenv("PORT"), "8080"),
		smtpHost:      os.Getenv("SMTP_HOST"),
		smtpPort:      firstNonEmpty(os.Getenv("SMTP_PORT"), "587"),
		smtpUsername:  os.Getenv("SMTP_USERNAME"),
		smtpPassword:  os.Getenv("SMTP_PASSWORD"),
		brevoAPIKey:   os.Getenv("BREVO_API_KEY"),
		mailFromEmail: firstNonEmpty(os.Getenv("MAIL_FROM_EMAIL"), os.Getenv("SMTP_USERNAME")),
		mailFromName:  firstNonEmpty(os.Getenv("MAIL_FROM_NAME"), "Adventskalender-Gewinnbenachrichtigung"),
	}
	if cfg.databaseURL == "" {
		return cfg, errors.New("DATABASE_URL is not set")
	}
	if cfg.baseURL == "" {
		cfg.devMode = true
		cfg.baseURL = "http://localhost:" + cfg.port
	}
	cfg.baseURL = strings.TrimSuffix(cfg.baseURL, "/")
	return cfg, nil
}

// errNoMailer means neither SMTP nor Brevo is configured.
var errNoMailer = errors.New("no mail service configured, set SMTP_HOST or BREVO_API_KEY")

// newMailer returns the SMTP or Brevo mailer. In development mode without
// either, it returns a mailer that only prints emails.
func (cfg config) newMailer() (Mailer, error) {
	switch {
	case cfg.smtpHost != "" || cfg.brevoAPIKey != "":
		if cfg.mailFromEmail == "" {
			return nil, errors.New("MAIL_FROM_EMAIL is not set")
		}
		if cfg.smtpHost != "" {
			return newSMTPMailer(cfg.smtpHost, cfg.smtpPort, cfg.smtpUsername, cfg.smtpPassword, cfg.mailFromEmail, cfg.mailFromName), nil
		}
		return newBrevoMailer(cfg.brevoAPIKey, cfg.mailFromEmail, cfg.mailFromName), nil
	case cfg.devMode:
		log.Print("no mail service configured, emails are printed instead of sent")
		return logMailer{}, nil
	default:
		return nil, errNoMailer
	}
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}
