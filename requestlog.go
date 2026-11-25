package main

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"strings"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

const createRequestLogTable = `
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

var requestLogColumns = []string{
	"received_at", "method", "path", "query", "status",
	"duration_ms", "client_ip", "user_agent", "referer",
}

const (
	requestLogQueueSize = 4096 // entries waiting to be written
	requestLogBatchSize = 500  // entries per COPY
	maxLoggedFieldBytes = 2048 // longer values are cut off
)

type requestLogEntry struct {
	receivedAt time.Time
	method     string
	path       string
	query      string
	status     int
	duration   time.Duration
	clientIP   string
	userAgent  string
	referer    string
}

// requestLogger writes one row per HTTP request to the request_log table.
// Requests only put an entry on a queue; a background goroutine writes the
// queue to the database in batches, so a slow database never slows down
// requests. When the queue is full, entries are dropped and counted.
type requestLogger struct {
	pool    *pgxpool.Pool
	entries chan requestLogEntry
	stop    chan struct{}
	done    chan struct{}
	dropped atomic.Int64
}

func newRequestLogger(ctx context.Context, databaseURL string) (*requestLogger, error) {
	config, err := pgxpool.ParseConfig(databaseURL)
	if err != nil {
		return nil, fmt.Errorf("parsing DATABASE_URL: %w", err)
	}
	config.MaxConns = 2

	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()

	pool, err := pgxpool.NewWithConfig(ctx, config)
	if err != nil {
		return nil, fmt.Errorf("connecting to database: %w", err)
	}
	if _, err := pool.Exec(ctx, createRequestLogTable); err != nil {
		pool.Close()
		return nil, fmt.Errorf("creating request_log table: %w", err)
	}

	l := &requestLogger{
		pool:    pool,
		entries: make(chan requestLogEntry, requestLogQueueSize),
		stop:    make(chan struct{}),
		done:    make(chan struct{}),
	}
	go l.run()
	return l, nil
}

// Middleware records every request once its response is complete. For event
// streams that is when the client disconnects.
func (l *requestLogger) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w}
		defer func() {
			l.enqueue(requestLogEntry{
				receivedAt: start,
				method:     r.Method,
				path:       r.URL.EscapedPath(),
				query:      r.URL.RawQuery,
				status:     rec.statusCode(),
				duration:   time.Since(start),
				clientIP:   clientIP(r),
				userAgent:  r.UserAgent(),
				referer:    r.Referer(),
			})
		}()
		next.ServeHTTP(rec, r)
	})
}

func (l *requestLogger) enqueue(e requestLogEntry) {
	select {
	case l.entries <- e:
	default:
		l.dropped.Add(1)
	}
}

func (l *requestLogger) run() {
	defer close(l.done)
	batch := make([]requestLogEntry, 0, requestLogBatchSize)
	for {
		select {
		case e := <-l.entries:
			batch = l.fill(append(batch[:0], e))
			l.write(batch)
		case <-l.stop:
			for {
				batch = l.fill(batch[:0])
				if len(batch) == 0 {
					return
				}
				l.write(batch)
			}
		}
	}
}

// fill adds queued entries to batch without waiting, up to the batch size.
func (l *requestLogger) fill(batch []requestLogEntry) []requestLogEntry {
	for len(batch) < requestLogBatchSize {
		select {
		case e := <-l.entries:
			batch = append(batch, e)
		default:
			return batch
		}
	}
	return batch
}

func (l *requestLogger) write(batch []requestLogEntry) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	_, err := l.pool.CopyFrom(ctx, pgx.Identifier{"request_log"}, requestLogColumns,
		pgx.CopyFromSlice(len(batch), func(i int) ([]any, error) {
			e := batch[i]
			return []any{
				e.receivedAt,
				cleanText(e.method),
				cleanText(e.path),
				cleanText(e.query),
				e.status,
				float64(e.duration) / float64(time.Millisecond),
				cleanText(e.clientIP),
				cleanText(e.userAgent),
				cleanText(e.referer),
			}, nil
		}))
	if err != nil {
		log.Printf("request log: writing %d entries: %v", len(batch), err)
	}
	if n := l.dropped.Swap(0); n > 0 {
		log.Printf("request log: queue full, dropped %d entries", n)
	}
}

// Close writes all queued entries and closes the database connections.
func (l *requestLogger) Close(ctx context.Context) {
	close(l.stop)
	select {
	case <-l.done:
	case <-ctx.Done():
		log.Printf("request log: shutdown timed out, %d entries not written", len(l.entries))
	}
	l.pool.Close()
}

// cleanText shortens s and makes it storable in a Postgres text column, which
// rejects invalid UTF-8 and NUL bytes. Headers can contain either.
func cleanText(s string) string {
	if len(s) > maxLoggedFieldBytes {
		s = s[:maxLoggedFieldBytes]
	}
	s = strings.ToValidUTF8(s, "�")
	return strings.ReplaceAll(s, "\x00", "")
}

// statusRecorder remembers the status code a handler sends.
type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (r *statusRecorder) WriteHeader(code int) {
	if r.status == 0 {
		r.status = code
	}
	r.ResponseWriter.WriteHeader(code)
}

func (r *statusRecorder) Write(b []byte) (int, error) {
	if r.status == 0 {
		r.status = http.StatusOK
	}
	return r.ResponseWriter.Write(b)
}

// Unwrap lets http.ResponseController reach the underlying writer, which
// the event stream needs for flushing.
func (r *statusRecorder) Unwrap() http.ResponseWriter {
	return r.ResponseWriter
}

func (r *statusRecorder) statusCode() int {
	if r.status == 0 {
		return http.StatusOK
	}
	return r.status
}
