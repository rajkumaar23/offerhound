package store

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/rajkumaar23/offerhound/internal/offer"
)

func newTestStore(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func TestOpenCreatesMissingParentDir(t *testing.T) {
	// fresh Docker volume: ~/.offerhound does not exist yet
	s, err := Open(filepath.Join(t.TempDir(), "nested", "deeper", "offers.db"))
	if err != nil {
		t.Fatalf("serve on a cold volume must not die: %v", err)
	}
	s.Close()
}

func sampleOffers(bank string) []offer.Offer {
	return []offer.Offer{
		{ID: "1", Bank: bank, CardLabel: "Venture X …1234", Merchant: "Chewy",
			RewardText: "Up to 7X miles", Notes: "7X applies to Autoship & Subscribe only; 1X everything else",
			Channel: "Online", Status: offer.Eligible, ScrapedAt: time.Now()},
		{ID: "2", Bank: bank, CardLabel: "Venture X …1234", Merchant: "Whole Foods",
			RewardText: "500 miles", Channel: "In-Store", Status: offer.Enrolled,
			ExpiryDate: "2030-01-01", ScrapedAt: time.Now()},
	}
}

func TestIngestAndSearch(t *testing.T) {
	s := newTestStore(t)
	if err := s.Ingest("amy", "c1", []offer.Card{{Label: "Venture X …1234"}},
		sampleOffers("c1"), true, "test"); err != nil {
		t.Fatal(err)
	}
	got, err := s.Search(Query{User: "amy", Text: "chewy"})
	if err != nil || len(got) != 1 || got[0].Merchant != "Chewy" {
		t.Fatalf("merchant search: %d %v", len(got), err)
	}
	// notes must be searchable — that's where the category fine print lives
	got, err = s.Search(Query{User: "amy", Text: "Autoship"})
	if err != nil || len(got) != 1 {
		t.Fatalf("notes search: %d %v", len(got), err)
	}
	got, _ = s.Search(Query{User: "amy", Channel: "in-store"})
	if len(got) != 1 || got[0].Merchant != "Whole Foods" {
		t.Fatalf("channel filter: %+v", got)
	}
	got, _ = s.Search(Query{User: "amy", Status: "enrolled"})
	if len(got) != 1 {
		t.Fatalf("status filter: %+v", got)
	}
	banks, _ := s.Banks("amy")
	if len(banks) != 1 || banks[0].Status != "ok" || len(banks[0].Cards) != 1 {
		t.Fatalf("bank health: %+v", banks[0])
	}
}

func TestWalletIsolation(t *testing.T) {
	s := newTestStore(t)
	s.Ingest("amy", "c1", nil, sampleOffers("c1"), true, "")
	s.Ingest("bob", "c1", nil, []offer.Offer{
		{ID: "9", Bank: "c1", Merchant: "Best Buy", RewardText: "$50", Status: offer.Eligible},
	}, true, "")
	if got, _ := s.Search(Query{User: "amy"}); len(got) != 2 {
		t.Fatalf("amy sees %d offers, want her own 2 only", len(got))
	}
	got, _ := s.Search(Query{User: "bob"})
	if len(got) != 1 || got[0].Merchant != "Best Buy" {
		t.Fatalf("bob's wallet contaminated: %+v", got)
	}
	users, _ := s.Users()
	if len(users) != 2 || users[0] != "amy" || users[1] != "bob" {
		t.Fatalf("Users(): %v", users)
	}
	// empty user falls back to the default wallet, never leaks others
	if got, _ := s.Search(Query{}); len(got) != 0 {
		t.Fatalf("default wallet must be empty, got %d", len(got))
	}
}

func TestPartialKeepsStale(t *testing.T) {
	s := newTestStore(t)
	s.Ingest("amy", "c1", nil, sampleOffers("c1"), true, "good")
	// a partial sync touching only one merchant must not drop the other
	if err := s.Ingest("amy", "c1", nil, []offer.Offer{
		{ID: "1", Bank: "c1", Merchant: "Chewy", RewardText: "8X miles", Status: offer.Eligible},
	}, false, "partial"); err != nil {
		t.Fatal(err)
	}
	all, _ := s.Search(Query{User: "amy"})
	if len(all) != 2 {
		t.Fatalf("stale offers dropped by partial: %d", len(all))
	}
	for _, o := range all {
		if o.Merchant == "Chewy" && o.RewardText != "8X miles" {
			t.Fatal("partial update not applied")
		}
	}
	banks, _ := s.Banks("amy")
	if len(banks) != 1 || banks[0].Status != "stale" {
		t.Fatalf("bank should be marked stale, got %+v", banks)
	}
	// failed full sync after good data => rejected
	err := s.Ingest("amy", "c1", nil, nil, true, "empty full sync")
	if err != ErrPartialRejected {
		t.Fatalf("want ErrPartialRejected, got %v", err)
	}
	all, _ = s.Search(Query{User: "amy"})
	if len(all) != 2 {
		t.Fatal("rejected sync must not touch data")
	}
}

func TestHumanRoundTrip(t *testing.T) {
	s := newTestStore(t)
	id, err := s.AskHuman("amy", "c1", "Enter the SMS code")
	if err != nil || id == 0 {
		t.Fatalf("ask: %d %v", id, err)
	}
	pending, _ := s.PendingHuman("amy")
	if len(pending) != 1 || pending[0].Prompt != "Enter the SMS code" {
		t.Fatalf("pending: %+v", pending)
	}
	if other, _ := s.PendingHuman("bob"); len(other) != 0 {
		t.Fatalf("human prompts must not cross wallets: %+v", other)
	}
	if ok, _ := s.AnswerHuman(id, "eve", "111111"); ok {
		t.Fatal("foreign user must not answer another wallet's prompt")
	}
	ok, err := s.AnswerHuman(id, "amy", "424242")
	if err != nil || !ok {
		t.Fatalf("answer: %v %v", ok, err)
	}
	got, err := s.WaitForAnswer(context.Background(), id, time.Second)
	if err != nil || got != "424242" {
		t.Fatalf("wait: %q %v", got, err)
	}
	if ok, _ := s.AnswerHuman(id, "amy", "000000"); ok {
		t.Fatal("double answer should be rejected")
	}
	answers, _ := s.AnswersFor([]int64{id})
	if len(answers) != 1 || answers[0].Answer != "424242" {
		t.Fatalf("AnswersFor: %+v", answers)
	}
	if pending, _ := s.PendingHuman("amy"); len(pending) != 0 {
		t.Fatalf("answered request still pending: %+v", pending)
	}
}

func TestExpiringSoonSortsFirst(t *testing.T) {
	s := newTestStore(t)
	s.Ingest("amy", "c1", nil, []offer.Offer{
		{ID: "a", Bank: "c1", Merchant: "Far", RewardText: "x", Status: offer.Eligible,
			ExpiryDate: "2030-06-01"},
		{ID: "b", Bank: "c1", Merchant: "Soon", RewardText: "y", Status: offer.Eligible,
			ExpiryDate: "2026-12-01"},
		{ID: "c", Bank: "c1", Merchant: "Gone", RewardText: "z", Status: offer.Eligible,
			ExpiryDate: "2020-01-01"},
	}, true, "")
	all, _ := s.Search(Query{User: "amy"})
	if len(all) != 3 || all[0].Merchant != "Soon" {
		t.Fatalf("expiring-soonest first: %+v", all)
	}
	if all[2].Merchant != "Gone" {
		t.Fatalf("expired last: %+v", all[2])
	}
}

func TestTokenHash(t *testing.T) {
	tok, err := NewToken()
	if err != nil || len(tok) != 48 {
		t.Fatalf("NewToken: %q %v", tok, err)
	}
	hash := TokenHash(tok)
	if !TokenHashMatches(hash, tok) {
		t.Fatal("correct token must match its hash")
	}
	if TokenHashMatches(hash, tok+"x") {
		t.Fatal("wrong token must not match")
	}
	if TokenHashMatches("not-hex", tok) || TokenHashMatches("", tok) {
		t.Fatal("malformed stored hash must never match")
	}
}
