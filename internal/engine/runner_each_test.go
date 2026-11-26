package engine

import (
	"context"
	"testing"

	"github.com/rajkumaar23/offerhound/internal/config"
)

// One canned document doubles as the /overview (accounts) and /offers
// responses: products.* carries the card items, o.* the extracted offers.
const eachCtxDoc = `{"products":{"TOK":{"product":{"description":"Blue Cash Everyday"},
"account":{"display_account_number":"31009"}}},
"o":{"offersList":{"page1":[{"merchant":"Mulberry - Luxury Handbags & Accessories","reward":"10% back"}]}}}`

func TestEachCardLabelContextAndCompleteFlag(t *testing.T) {
	r := &Runner{
		Bank: &config.Bank{
			ID:      "t",
			BaseURL: "https://example.test",
			Offers: []config.Step{
				{FetchAPI: &config.FetchAPISpec{URL: "/overview", SaveAs: "overview"}},
				{FetchAPI: &config.FetchAPISpec{
					URL: "/offers",
					Each: &config.EachSpec{
						From: "overview", Items: "products.*", As: "acct",
						CardLabel: "{{.product.description}} …{{.account.display_account_number}}",
					},
					Extract: &config.Extract{
						Items:  "o.offersList.page1[*]",
						Status: "eligible",
						Fields: map[string]config.FieldMap{
							"merchant":   {Path: "merchant", Transform: "beforeDash"},
							"rewardText": {Path: "reward"},
						},
					},
				}},
			},
		},
		Browser: &fakeBrowser{body: eachCtxDoc, status: 200},
	}
	res := r.Run(context.Background())
	if res.Err != nil {
		t.Fatalf("run: %v", res.Err)
	}
	if !res.Complete {
		t.Fatalf("clean run must be Complete (regression: flag never set true)")
	}
	if len(res.Offers) != 1 {
		t.Fatalf("offers = %d, want 1", len(res.Offers))
	}
	o := res.Offers[0]
	if o.CardLabel != "Blue Cash Everyday …31009" {
		t.Fatalf("cardLabel rendered over wrong context: %q", o.CardLabel)
	}
	if o.Merchant != "Mulberry" {
		t.Fatalf("beforeDash must cut the category tag: %q", o.Merchant)
	}
}

func TestCompleteFalseWhenNoOffers(t *testing.T) {
	r := &Runner{
		Bank: &config.Bank{ID: "t", BaseURL: "https://example.test",
			Offers: []config.Step{{FetchAPI: &config.FetchAPISpec{URL: "/x"}}}},
		Browser: &fakeBrowser{body: `{"nothing":1}`, status: 200},
	}
	if res := r.Run(context.Background()); res.Complete {
		t.Fatal("offer-less run must not be flagged complete")
	}
}
