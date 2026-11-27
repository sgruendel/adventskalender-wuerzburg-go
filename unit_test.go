package main

import (
	"context"
	"encoding/json/v2"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
)

func TestNormalizeEmail(t *testing.T) {
	tests := []struct {
		in   string
		want string
		ok   bool
	}{
		{"max@example.de", "max@example.de", true},
		{"  Max.Muster@Example.DE ", "max.muster@example.de", true},
		{"a+b@sub.example.com", "a+b@sub.example.com", true},
		{"", "", false},
		{"max", "", false},
		{"max@localhost", "", false},
		{"max@example.", "", false},
		{"Max <max@example.de>", "", false},
		{"max@example.de, other@example.de", "", false},
		{"max@exa mple.de", "", false},
		{"max@example.de\r\nBcc: x@example.de", "", false},
	}
	for _, tt := range tests {
		got, ok := normalizeEmail(tt.in)
		if got != tt.want || ok != tt.ok {
			t.Errorf("normalizeEmail(%q) = %q, %v; want %q, %v", tt.in, got, ok, tt.want, tt.ok)
		}
	}
}

func TestParseCalendarNumber(t *testing.T) {
	tests := []struct {
		in   string
		want int
		ok   bool
	}{
		{"1234", 1234, true},
		{"0640", 640, true},
		{"640", 640, true},
		{" 7 ", 7, true},
		{"0000", 0, true},
		{"", 0, false},
		{"12345", 0, false},
		{"12a4", 0, false},
		{"-123", 0, false},
		{"１２３", 0, false}, // full-width digits
	}
	for _, tt := range tests {
		got, ok := parseCalendarNumber(tt.in)
		if got != tt.want || ok != tt.ok {
			t.Errorf("parseCalendarNumber(%q) = %d, %v; want %d, %v", tt.in, got, ok, tt.want, tt.ok)
		}
	}
}

// fakePrizeAPI serves entries like the organizer's API: pages of 10,
// selected with the offset parameter.
func fakePrizeAPI(t *testing.T, entries []map[string]any) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("year") != "2025" {
			w.Write([]byte(`{"prizes":[],"count":0}`))
			return
		}
		offset, _ := strconv.Atoi(r.URL.Query().Get("offset"))
		end := min(offset+10, len(entries))
		page := entries[min(offset, len(entries)):end]
		w.Header().Set("Content-Type", "application/json")
		json.MarshalWrite(w, map[string]any{"prizes": page, "count": len(entries)})
	}))
}

func apiEntry(id int, number string, day string) map[string]any {
	return map[string]any{
		"count": 1, "id": id, "kalendernummer": number,
		"name": "Sponsor " + strconv.Itoa(id), "bezeichnung": "Preis " + strconv.Itoa(id),
		"tag": day, "monat": "12", "jahr": 2025, "wert": "10.00", "sponsorId": 1,
	}
}

func TestPrizeFetcher(t *testing.T) {
	var entries []map[string]any
	for i := range 25 {
		entries = append(entries, apiEntry(1000+i, fmt.Sprintf("%04d", i*7), fmt.Sprintf("%02d", i/3+1)))
	}
	entries[12]["kalendernummer"] = "abc" // must be skipped without shifting later pages

	api := fakePrizeAPI(t, entries)
	defer api.Close()
	f := newPrizeFetcher()
	f.url = api.URL

	prizes, err := f.Fetch(context.Background(), 2025)
	if err != nil {
		t.Fatal(err)
	}
	if len(prizes) != 24 {
		t.Fatalf("got %d prizes, want 24", len(prizes))
	}
	last := prizes[len(prizes)-1]
	want := Prize{ID: 1024, Year: 2025, Month: 12, Day: 9, CalendarNumber: 168, Description: "Preis 1024", Sponsor: "Sponsor 1024"}
	if last != want {
		t.Errorf("last prize = %+v, want %+v", last, want)
	}
	for _, p := range prizes {
		if p.ID == 1012 {
			t.Error("prize with invalid calendar number was not skipped")
		}
	}

	none, err := f.Fetch(context.Background(), 2026)
	if err != nil || len(none) != 0 {
		t.Errorf("Fetch(2026) = %v, %v; want no prizes", none, err)
	}
}

func TestPrizeFetcherRejectsChangedFormat(t *testing.T) {
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`<html>Wartungsarbeiten</html>`))
	}))
	defer api.Close()
	f := newPrizeFetcher()
	f.url = api.URL
	if _, err := f.Fetch(context.Background(), 2025); err == nil {
		t.Error("expected an error for a non-JSON response")
	}
}

func TestBrevoMailer(t *testing.T) {
	var got map[string]any
	var apiKey string
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		apiKey = r.Header.Get("api-key")
		body, _ := io.ReadAll(r.Body)
		if err := json.Unmarshal(body, &got); err != nil {
			t.Errorf("invalid JSON: %v", err)
		}
		w.WriteHeader(http.StatusCreated)
		w.Write([]byte(`{"messageId":"<1@relay>"}`))
	}))
	defer api.Close()

	m := newBrevoMailer("secret-key", "noreply@example.de", "Absender")
	m.url = api.URL
	err := m.Send(context.Background(), Mail{
		To: "max@example.de", Subject: "Grüße", Text: "Hallo", UnsubscribeURL: "https://x.example/abmelden?token=t",
	})
	if err != nil {
		t.Fatal(err)
	}
	if apiKey != "secret-key" {
		t.Errorf("api-key header = %q", apiKey)
	}
	want := map[string]any{
		"sender":      map[string]any{"email": "noreply@example.de", "name": "Absender"},
		"to":          []any{map[string]any{"email": "max@example.de"}},
		"subject":     "Grüße",
		"textContent": "Hallo",
		"headers": map[string]any{
			"List-Unsubscribe":      "<https://x.example/abmelden?token=t>",
			"List-Unsubscribe-Post": "List-Unsubscribe=One-Click",
		},
	}
	gotJSON, _ := json.Marshal(got, json.Deterministic(true))
	wantJSON, _ := json.Marshal(want, json.Deterministic(true))
	if string(gotJSON) != string(wantJSON) {
		t.Errorf("request body\n got: %s\nwant: %s", gotJSON, wantJSON)
	}
}

func TestBrevoMailerError(t *testing.T) {
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		w.Write([]byte(`{"code":"invalid_parameter","message":"sender is invalid"}`))
	}))
	defer api.Close()
	m := newBrevoMailer("k", "noreply@example.de", "")
	m.url = api.URL
	err := m.Send(context.Background(), Mail{To: "max@example.de", Subject: "s", Text: "t"})
	if err == nil {
		t.Fatal("expected an error")
	}
	t.Log(err)
}
