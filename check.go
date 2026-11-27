package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// runCheck implements "app check": it loads the prizes published so far,
// stores them, and emails every confirmed calendar owner about prizes they
// have not been told about yet. Running it again sends nothing new, so it
// is safe to schedule it several times a day.
func runCheck(ctx context.Context, args []string) error {
	flags := flag.NewFlagSet("check", flag.ContinueOnError)
	year := flags.Int("year", time.Now().Year(), "season to check")
	dryRun := flags.Bool("dry-run", false, "print the emails that would be sent, change nothing")
	maxMails := flags.Int("max-mails", 100, "send nothing if more emails than this are due (safety limit)")
	if err := flags.Parse(args); err != nil {
		return err
	}

	cfg, err := loadConfig()
	if err != nil {
		return err
	}
	var mailer Mailer = logMailer{}
	if !*dryRun {
		if mailer, err = cfg.newMailer(); err != nil {
			return err
		}
		if _, ok := mailer.(logMailer); ok {
			return fmt.Errorf("%w; use -dry-run to only print the emails", errNoMailer)
		}
	}
	db, err := openDB(ctx, cfg.databaseURL, 2)
	if err != nil {
		return err
	}
	defer db.Close()

	c := &checker{
		db:          db,
		mailer:      mailer,
		fetchPrizes: newPrizeFetcher().Fetch,
		baseURL:     cfg.baseURL,
	}
	return c.run(ctx, *year, *dryRun, *maxMails)
}

type checker struct {
	db          *pgxpool.Pool
	mailer      Mailer
	fetchPrizes func(ctx context.Context, year int) ([]Prize, error)
	baseURL     string
}

// winner is a confirmed calendar with prizes its owner has not been told about.
type winner struct {
	calendarID       int64
	number           int
	email            string
	unsubscribeToken string
	prizes           []Prize
}

func (c *checker) run(ctx context.Context, year int, dryRun bool, maxMails int) error {
	prizes, err := c.fetchPrizes(ctx, year)
	if err != nil {
		return err
	}
	log.Printf("%d prizes published for %d", len(prizes), year)

	tx, err := c.db.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)

	if !dryRun {
		if err := purgeUnconfirmed(ctx, tx); err != nil {
			return err
		}
	}
	if err := storePrizes(ctx, tx, prizes); err != nil {
		return err
	}
	winners, err := findWinners(ctx, tx, year)
	if err != nil {
		return err
	}
	log.Printf("%d emails due", len(winners))

	if dryRun {
		// The transaction is rolled back, so the database stays unchanged.
		for _, w := range winners {
			c.mailer.Send(ctx, winnerMail(w.email, w.number, w.prizes, c.unsubscribeURL(w.unsubscribeToken)))
		}
		return nil
	}
	if err := tx.Commit(ctx); err != nil {
		return err
	}
	if len(winners) > maxMails {
		return fmt.Errorf("%d emails due, more than the limit of %d; check the prize data, then rerun with a higher -max-mails", len(winners), maxMails)
	}

	var failed int
	for _, w := range winners {
		mail := winnerMail(w.email, w.number, w.prizes, c.unsubscribeURL(w.unsubscribeToken))
		if err := c.mailer.Send(ctx, mail); err != nil {
			log.Printf("calendar %s: %v", formatNumber(w.number), err)
			failed++
			continue
		}
		if err := recordNotifications(ctx, c.db, w); err != nil {
			// The email went out, but the next run will send it again.
			log.Printf("calendar %s: email sent, but not recorded: %v", formatNumber(w.number), err)
			failed++
		}
	}
	log.Printf("%d emails sent, %d failed", len(winners)-failed, failed)
	if failed > 0 {
		return fmt.Errorf("%d of %d emails failed", failed, len(winners))
	}
	return nil
}

func (c *checker) unsubscribeURL(token string) string {
	return c.baseURL + "/abmelden?token=" + token
}

// purgeUnconfirmed deletes registrations that were not confirmed in time,
// and users left without calendars.
func purgeUnconfirmed(ctx context.Context, db dbtx) error {
	tag, err := db.Exec(ctx, `
		DELETE FROM calendars
		WHERE confirmed_at IS NULL
		  AND confirmation_sent_at < now() - $1 * interval '1 second'`,
		confirmationValidFor.Seconds())
	if err != nil {
		return fmt.Errorf("deleting unconfirmed calendars: %w", err)
	}
	if n := tag.RowsAffected(); n > 0 {
		log.Printf("deleted %d unconfirmed registrations", n)
	}
	_, err = db.Exec(ctx, `
		DELETE FROM users u
		WHERE created_at < now() - interval '1 hour'
		  AND NOT EXISTS (SELECT 1 FROM calendars c WHERE c.user_id = u.id)`)
	if err != nil {
		return fmt.Errorf("deleting users without calendars: %w", err)
	}
	return nil
}

func storePrizes(ctx context.Context, db dbtx, prizes []Prize) error {
	batch := &pgx.Batch{}
	for _, p := range prizes {
		batch.Queue(`
			INSERT INTO prizes (id, year, month, day, calendar_number, description, sponsor)
			VALUES ($1, $2, $3, $4, $5, $6, $7)
			ON CONFLICT (id) DO UPDATE SET
				year = EXCLUDED.year,
				month = EXCLUDED.month,
				day = EXCLUDED.day,
				calendar_number = EXCLUDED.calendar_number,
				description = EXCLUDED.description,
				sponsor = EXCLUDED.sponsor,
				updated_at = now()
			WHERE (prizes.year, prizes.month, prizes.day, prizes.calendar_number, prizes.description, prizes.sponsor)
			      IS DISTINCT FROM
			      (EXCLUDED.year, EXCLUDED.month, EXCLUDED.day, EXCLUDED.calendar_number, EXCLUDED.description, EXCLUDED.sponsor)`,
			p.ID, p.Year, p.Month, p.Day, p.CalendarNumber, p.Description, p.Sponsor)
	}
	if err := db.SendBatch(ctx, batch).Close(); err != nil {
		return fmt.Errorf("storing prizes: %w", err)
	}
	return nil
}

// findWinners returns the confirmed calendars that won prizes in the given
// year that their owners have not been emailed about yet.
func findWinners(ctx context.Context, db dbtx, year int) ([]winner, error) {
	rows, err := db.Query(ctx, `
		SELECT c.id, c.number, u.email, c.unsubscribe_token,
		       p.id, p.year, p.month, p.day, p.calendar_number, p.description, p.sponsor
		FROM calendars c
		JOIN users u ON u.id = c.user_id
		JOIN prizes p ON p.calendar_number = c.number AND p.year = $1
		WHERE c.confirmed_at IS NOT NULL
		  AND NOT EXISTS (
			SELECT 1 FROM notifications n
			WHERE n.calendar_id = c.id AND n.prize_id = p.id)
		ORDER BY c.id, p.month, p.day, p.id`, year)
	if err != nil {
		return nil, fmt.Errorf("finding winners: %w", err)
	}
	defer rows.Close()

	var winners []winner
	for rows.Next() {
		var w winner
		var p Prize
		err := rows.Scan(&w.calendarID, &w.number, &w.email, &w.unsubscribeToken,
			&p.ID, &p.Year, &p.Month, &p.Day, &p.CalendarNumber, &p.Description, &p.Sponsor)
		if err != nil {
			return nil, err
		}
		if n := len(winners); n > 0 && winners[n-1].calendarID == w.calendarID {
			winners[n-1].prizes = append(winners[n-1].prizes, p)
			continue
		}
		w.prizes = []Prize{p}
		winners = append(winners, w)
	}
	return winners, rows.Err()
}

func recordNotifications(ctx context.Context, db dbtx, w winner) error {
	batch := &pgx.Batch{}
	for _, p := range w.prizes {
		batch.Queue(`INSERT INTO notifications (calendar_id, prize_id) VALUES ($1, $2) ON CONFLICT DO NOTHING`,
			w.calendarID, p.ID)
	}
	return db.SendBatch(ctx, batch).Close()
}
