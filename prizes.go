package main

import (
	"context"
	"encoding/json/v2"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// Prize is one drawn calendar number and what it won.
type Prize struct {
	ID             int64
	Year           int
	Month          int
	Day            int
	CalendarNumber int
	Description    string
	Sponsor        string
}

// prizeAPIURL is the JSON endpoint behind the organizer's winners table
// (www.adventskalender-wuerzburg.de/gewinne). It returns the prizes of a
// year in pages of 10, selected with the offset parameter. The format was
// taken from the 2025 edition; the organizer may change it.
const prizeAPIURL = "https://www.adventskalender-wuerzburg.de/api/gewinns/table"

const (
	maxPrizePages  = 200                    // stop paging after this many requests
	prizePageDelay = 200 * time.Millisecond // pause between requests, to be polite
)

type prizeAPIPage struct {
	Prizes []prizeAPIEntry `json:"prizes"`
	Count  int             `json:"count"`
}

type prizeAPIEntry struct {
	ID             int64  `json:"id"`
	Kalendernummer string `json:"kalendernummer"`
	Bezeichnung    string `json:"bezeichnung"`
	Name           string `json:"name"`
	Tag            string `json:"tag"`
	Monat          string `json:"monat"`
	Jahr           int    `json:"jahr"`
}

type prizeFetcher struct {
	url    string
	client *http.Client
}

func newPrizeFetcher() *prizeFetcher {
	return &prizeFetcher{url: prizeAPIURL, client: &http.Client{Timeout: 30 * time.Second}}
}

// Fetch returns all prizes published so far for the given year.
func (f *prizeFetcher) Fetch(ctx context.Context, year int) ([]Prize, error) {
	var prizes []Prize
	for page := 0; page < maxPrizePages; page++ {
		if page > 0 {
			select {
			case <-time.After(prizePageDelay):
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}
		offset := len(prizes)
		result, err := f.fetchPage(ctx, year, offset)
		if err != nil {
			return nil, fmt.Errorf("fetching prizes at offset %d: %w", offset, err)
		}
		for _, e := range result.Prizes {
			p, err := e.toPrize()
			if err != nil {
				log.Printf("skipping prize %d: %v", e.ID, err)
				// Still count it, so the offset of the next page is right.
				p = Prize{ID: -1}
			}
			prizes = append(prizes, p)
		}
		if len(result.Prizes) == 0 || len(prizes) >= result.Count {
			return withoutSkipped(prizes), nil
		}
	}
	return nil, fmt.Errorf("more than %d pages of prizes, giving up", maxPrizePages)
}

func (f *prizeFetcher) fetchPage(ctx context.Context, year, offset int) (*prizeAPIPage, error) {
	q := url.Values{"year": {strconv.Itoa(year)}}
	if offset > 0 {
		q.Set("offset", strconv.Itoa(offset))
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, f.url+"?"+q.Encode(), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "adventskalender-gewinnbenachrichtigung/1.0")

	resp, err := f.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("unexpected status %s", resp.Status)
	}
	var page prizeAPIPage
	if err := json.UnmarshalRead(io.LimitReader(resp.Body, 1<<20), &page); err != nil {
		return nil, fmt.Errorf("decoding response: %w", err)
	}
	return &page, nil
}

func (e prizeAPIEntry) toPrize() (Prize, error) {
	number, err := strconv.Atoi(strings.TrimSpace(e.Kalendernummer))
	if err != nil || number < 0 || number > 9999 {
		return Prize{}, fmt.Errorf("invalid calendar number %q", e.Kalendernummer)
	}
	day, err := strconv.Atoi(strings.TrimSpace(e.Tag))
	if err != nil || day < 1 || day > 31 {
		return Prize{}, fmt.Errorf("invalid day %q", e.Tag)
	}
	month, err := strconv.Atoi(strings.TrimSpace(e.Monat))
	if err != nil || month < 1 || month > 12 {
		return Prize{}, fmt.Errorf("invalid month %q", e.Monat)
	}
	return Prize{
		ID:             e.ID,
		Year:           e.Jahr,
		Month:          month,
		Day:            day,
		CalendarNumber: number,
		Description:    strings.TrimSpace(e.Bezeichnung),
		Sponsor:        strings.TrimSpace(e.Name),
	}, nil
}

func withoutSkipped(prizes []Prize) []Prize {
	out := prizes[:0]
	for _, p := range prizes {
		if p.ID != -1 {
			out = append(out, p)
		}
	}
	return out
}
