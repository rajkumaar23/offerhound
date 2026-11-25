package engine

import (
	"testing"
	"time"

	"github.com/rajkumaar23/offerhound/internal/config"
)

func TestMapOffers(t *testing.T) {
	doc := []byte(`{"data":[
	  {"merchantDisplayName":"Chewy","buttonText":"7X miles","text":"Online",
	   "subText":"7X on autoship only","offerSources":["affiliate"],"badge":{"text":"7 days left"}},
	  {"merchantDisplayName":"","buttonText":"broken"},
	  {"merchantDisplayName":"Target","buttonText":"Spend $20,\n earn 500 miles","text":"In-Store & Online"}
	]}`)
	ex := &config.Extract{
		Items:  "$.data[*]",
		Status: "eligible",
		Fields: map[string]config.FieldMap{
			"merchant":   {Path: "merchantDisplayName"},
			"rewardText": {Path: "buttonText"},
			"channel":    {Path: "text"},
			"notes":      {Path: "subText"},
			"source":     {Path: "offerSources", First: true},
			"expiryDate": {Path: "badge.text", Transform: "daysLeftToDate"},
		},
	}
	offers, err := MapOffers("c1", "Venture …1234", doc, ex, time.Unix(0, 0).UTC())
	if err != nil {
		t.Fatal(err)
	}
	if len(offers) != 2 {
		t.Fatalf("malformed item should be skipped, got %d", len(offers))
	}
	o := offers[0]
	if o.Merchant != "Chewy" || o.RewardText != "7X miles" || o.RewardValue != 7 {
		t.Fatalf("bad mapping: %+v", o)
	}
	if o.Notes != "7X on autoship only" || o.Source != "affiliate" {
		t.Fatalf("notes/source mapping: %+v", o)
	}
	if o.Status != "eligible" || o.CardLabel != "Venture …1234" {
		t.Fatalf("status/card: %+v", o)
	}
	if _, err := time.Parse("2006-01-02", o.ExpiryDate); err != nil {
		t.Fatalf("daysLeftToDate produced %q", o.ExpiryDate)
	}
	if offers[1].RewardText != "Spend $20, earn 500 miles" {
		t.Fatalf("newlines not collapsed: %q", offers[1].RewardText)
	}
	if offers[0].ID != OfferID(offers[0]) {
		t.Fatal("id not stable")
	}
}

func TestParseRewardValue(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want float64
	}{
		{"7X miles", 7}, {"Up to 14X miles", 14}, {"$5 back", 5},
		{"Spend $20, earn 500 miles", 20}, {"10% back", 10}, {"", 0},
	} {
		if got := parseRewardValue(tc.in); got != tc.want {
			t.Errorf("parseRewardValue(%q)=%v want %v", tc.in, got, tc.want)
		}
	}
}

func TestRenderTemplate(t *testing.T) {
	out, err := renderTemplate("/feed/{{urlquery .acct.id}}?v={{uuid}}", map[string]any{
		"acct": map[string]any{"id": "aB/+="},
	})
	if err != nil {
		t.Fatal(err)
	}
	if want := "/feed/aB%2F%2B%3D"; len(out) < len(want) || out[:len(want)] != want {
		t.Fatalf("got %q", out)
	}
}
