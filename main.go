// Command app notifies owners of a Würzburg Lions advent calendar by email
// when their calendar number is drawn.
//
//	app          runs the web service (registration with double opt-in)
//	app check    fetches the published prizes and emails the winners;
//	             meant to run hourly from cron in December
package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"
)

func main() {
	// Docker sends SIGTERM before it stops the container.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	command, args := "serve", os.Args[1:]
	if len(args) > 0 {
		command, args = args[0], args[1:]
	}

	var err error
	switch command {
	case "serve":
		err = serve(ctx)
	case "check":
		err = runCheck(ctx, args)
	default:
		err = fmt.Errorf("unknown command %q, use \"serve\" or \"check\"", command)
	}
	if err != nil {
		log.Fatal(err)
	}
}

func serve(ctx context.Context) error {
	cfg, err := loadConfig()
	if err != nil {
		return err
	}
	mailer, err := cfg.newMailer()
	if err != nil {
		return err
	}
	db, err := openDB(ctx, cfg.databaseURL, 4)
	if err != nil {
		return err
	}
	defer db.Close()

	app, err := newServer(db, mailer, cfg.baseURL)
	if err != nil {
		return err
	}
	requestLog := newRequestLogger(db)
	handler := requestLog.Middleware(securityHeaders(recovery(app.routes())))

	// Request contexts are cancelled when shutdown starts, so slow requests
	// do not keep the shutdown waiting.
	requestCtx, cancelRequests := context.WithCancel(context.Background())
	server := &http.Server{
		Addr:              ":" + cfg.port,
		Handler:           handler,
		ReadHeaderTimeout: 10 * time.Second,
		BaseContext:       func(net.Listener) context.Context { return requestCtx },
	}
	server.RegisterOnShutdown(cancelRequests)

	serveErr := make(chan error, 1)
	go func() { serveErr <- server.ListenAndServe() }()
	log.Printf("listening on :%s, links point to %s", cfg.port, cfg.baseURL)

	select {
	case err := <-serveErr:
		return err
	case <-ctx.Done():
	}

	log.Print("shutting down")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if err := server.Shutdown(shutdownCtx); err != nil && !errors.Is(err, context.DeadlineExceeded) {
		log.Printf("shutdown: %v", err)
	}
	requestLog.Close(shutdownCtx)
	return nil
}
