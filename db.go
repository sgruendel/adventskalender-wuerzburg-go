package main

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

const schema = `
-- People who registered, identified by their email address.
CREATE TABLE IF NOT EXISTS users (
	id         bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
	email      text        NOT NULL UNIQUE,
	created_at timestamptz NOT NULL DEFAULT now()
);

-- Calendars registered for notifications. A row is active once the owner
-- has confirmed it through the link in the confirmation email (double opt-in).
CREATE TABLE IF NOT EXISTS calendars (
	id                   bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
	user_id              bigint      NOT NULL REFERENCES users ON DELETE CASCADE,
	number               integer     NOT NULL CHECK (number BETWEEN 0 AND 9999),
	confirm_token_hash   bytea       UNIQUE,
	confirmation_sent_at timestamptz NOT NULL,
	confirmed_at         timestamptz,
	unsubscribe_token    text        NOT NULL UNIQUE,
	created_at           timestamptz NOT NULL DEFAULT now(),
	UNIQUE (user_id, number)
);
CREATE INDEX IF NOT EXISTS calendars_number_idx ON calendars (number);

-- Prizes as published by the organizer; id is the organizer's id.
CREATE TABLE IF NOT EXISTS prizes (
	id              bigint      PRIMARY KEY,
	year            integer     NOT NULL,
	month           integer     NOT NULL,
	day             integer     NOT NULL,
	calendar_number integer     NOT NULL,
	description     text        NOT NULL,
	sponsor         text        NOT NULL,
	first_seen_at   timestamptz NOT NULL DEFAULT now(),
	updated_at      timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS prizes_year_number_idx ON prizes (year, calendar_number);

-- Which calendar owner was told about which prize, so nobody is emailed twice.
CREATE TABLE IF NOT EXISTS notifications (
	calendar_id bigint      NOT NULL REFERENCES calendars ON DELETE CASCADE,
	prize_id    bigint      NOT NULL REFERENCES prizes ON DELETE CASCADE,
	sent_at     timestamptz NOT NULL DEFAULT now(),
	PRIMARY KEY (calendar_id, prize_id)
);

-- One row per HTTP request to the web service.
CREATE TABLE IF NOT EXISTS request_log (
	id          bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
	received_at timestamptz      NOT NULL,
	method      text             NOT NULL,
	path        text             NOT NULL,
	query       text             NOT NULL,
	status      integer          NOT NULL,
	duration_ms double precision NOT NULL,
	client_ip   text             NOT NULL,
	user_agent  text             NOT NULL,
	referer     text             NOT NULL
);
CREATE INDEX IF NOT EXISTS request_log_received_at_idx ON request_log (received_at);
`

// schemaLockID serializes schema creation between the web service and the
// cron job when both start at the same time.
const schemaLockID = 7_201_224

// dbtx is implemented by both *pgxpool.Pool and pgx.Tx.
type dbtx interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
	SendBatch(ctx context.Context, b *pgx.Batch) pgx.BatchResults
}

// openDB connects to Postgres and creates any missing tables.
func openDB(ctx context.Context, databaseURL string, maxConns int32) (*pgxpool.Pool, error) {
	config, err := pgxpool.ParseConfig(databaseURL)
	if err != nil {
		return nil, fmt.Errorf("parsing DATABASE_URL: %w", err)
	}
	config.MaxConns = maxConns

	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	db, err := pgxpool.NewWithConfig(ctx, config)
	if err != nil {
		return nil, fmt.Errorf("connecting to database: %w", err)
	}
	err = pgx.BeginFunc(ctx, db, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, "SELECT pg_advisory_xact_lock($1)", schemaLockID); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, schema)
		return err
	})
	if err != nil {
		db.Close()
		return nil, fmt.Errorf("creating database schema: %w", err)
	}
	return db, nil
}
