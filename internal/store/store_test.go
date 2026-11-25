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
	if err := s.Ingest("c1", []offer.Card{{Label: "Venture X …1234"}},
		sampleOffers("c1"), true, "test"); err != nil {
		t.Fatal(err)
	}
	got, err := s.Search(Query{Text: "chewy"})
	if err != nil || len(got) != 1 || got[0].Merchant != "Chewy" {
		t.Fatalf("merchant search: %d %v", len(got), err)
	}
	// notes must be searchable — that's where the category fine print lives
	got, err = s.Search(Query{Text: "Autoship"})
	if err != nil || len(got) != 1 {
		t.Fatalf("notes search: %d %v", len(got), err)
	}
	got, _ = s.Search(Query{Channel: "in-store"})
	if len(got) != 1 || got[0].Merchant != "Whole Foods" {
		t.Fatalf("channel filter: %+v", got)
	}
	got, _ = s.Search(Query{Status: "enrolled"})
	if len(got) != 1 {
		t.Fatalf("status filter: %+v", got)
	}
	banks, _ := s.Banks()
	if len(banks) != 1 || banks[0].Status != "ok" || len(banks[0].Cards) != 1 {
		t.Fatalf("bank health: %+v", banks[0])
	}
}

func TestPartialKeepsStale(t *testing.T) {
	s := newTestStore(t)
	s.Ingest("c1", nil, sampleOffers("c1"), true, "good")
	// a partial sync touching only one merchant must not drop the other
	if err := s.Ingest("c1", nil, []offer.Offer{
		{ID: "1", Bank: "c1", Merchant: "Chewy", RewardText: "8X miles", Status: offer.Eligible},
	}, false, "partial"); err != nil {
		t.Fatal(err)
	}
	all, _ := s.Search(Query{})
	if len(all) != 2 {
		t.Fatalf("stale offers dropped by partial: %d", len(all))
	}
	for _, o := range all {
		if o.Merchant == "Chewy" && o.RewardText != "8X miles" {
			t.Fatal("partial update not applied")
		}
	}
	banks, _ := s.Banks()
	if len(banks) != 1 || banks[0].Status != "stale" {
		t.Fatalf("bank should be marked stale, got %+v", banks)
	}
	// failed full sync after good data => rejected
	err := s.Ingest("c1", nil, nil, true, "empty full sync")
	if err != ErrPartialRejected {
		t.Fatalf("want ErrPartialRejected, got %v", err)
	}
	all, _ = s.Search(Query{})
	if len(all) != 2 {
		t.Fatal("rejected sync must not touch data")
	}
}

func TestJobsAndHumanRoundTrip(t *testing.T) {
	s := newTestStore(t)
	if _, err := s.EnqueueJob("c1"); err != nil {
		t.Fatal(err)
	}
	j, err := s.ClaimJob("w1", []string{"other"})
	if j != nil || err != nil {
		t.Fatalf("bank filter respected: %v", j)
	}
	j, _ = s.ClaimJob("w1", []string{"c1"})
	if j == nil {
		t.Fatal("job not claimed")
	}
	runID, _ := s.StartRun("c1", "w1")
	id, err := s.AskHuman(runID, "c1", "Enter the SMS code", nil)
	if err != nil || id == 0 {
		t.Fatalf("ask: %d %v", id, err)
	}
	ok, err := s.AnswerHuman(id, "424242")
	if err != nil || !ok {
		t.Fatalf("answer: %v %v", ok, err)
	}
	got, err := s.WaitForAnswer(context.Background(), id, time.Second)
	if err != nil || got != "424242" {
		t.Fatalf("wait: %q %v", got, err)
	}
	if ok, _ := s.AnswerHuman(id, "000000"); ok {
		t.Fatal("double answer should be rejected")
	}
	answers, _ := s.AnswersFor([]int64{id})
	if len(answers) != 1 || answers[0].Answer != "424242" {
		t.Fatalf("AnswersFor: %+v", answers)
	}
	if pending, _ := s.PendingHuman(); len(pending) != 0 {
		t.Fatalf("answered request still pending: %+v", pending)
	}
	if err := s.CompleteJob(j.ID, "done", ""); err != nil {
		t.Fatal(err)
	}
}

func TestExpiringSoonSortsFirst(t *testing.T) {
	s := newTestStore(t)
	s.Ingest("c1", nil, []offer.Offer{
		{ID: "a", Bank: "c1", Merchant: "Far", RewardText: "x", Status: offer.Eligible,
			ExpiryDate: "2030-06-01"},
		{ID: "b", Bank: "c1", Merchant: "Soon", RewardText: "y", Status: offer.Eligible,
			ExpiryDate: "2026-12-01"},
		{ID: "c", Bank: "c1", Merchant: "Gone", RewardText: "z", Status: offer.Eligible,
			ExpiryDate: "2020-01-01"},
	}, true, "")
	all, _ := s.Search(Query{})
	if len(all) != 3 || all[0].Merchant != "Soon" {
		t.Fatalf("expiring-soonest first: %+v", all)
	}
	if all[2].Merchant != "Gone" {
		t.Fatalf("expired last: %+v", all[2])
	}
}
