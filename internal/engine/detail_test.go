package engine

import (
	"context"
	"encoding/json"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/rajkumaar23/offerhound/internal/config"
)

// urlFake answers fetch envelopes based on which URL is embedded in the
// generated JS expression.
type urlFake struct {
	pages map[string]string // substring -> response body
	calls []string          // raw exprs seen (debug)
}

func (f *urlFake) Navigate(context.Context, string) error { return nil }
func (f *urlFake) EvaluateJSON(_ context.Context, expr string) (string, error) {
	f.calls = append(f.calls, expr)
	body, status := "", 404
	best := -1
	for sub, b := range f.pages {
		if strings.Contains(expr, sub) && len(sub) > best {
			body, status, best = b, 200, len(sub)
		}
	}
	env, _ := json.Marshal(struct {
		Status     int    `json:"status"`
		RetryAfter string `json:"retryAfter"`
		Body       string `json:"body"`
	}{status, "", body})
	return string(env), nil
}
func (f *urlFake) WaitFor(context.Context, string, string, bool, time.Duration) error { return nil }
func (f *urlFake) Click(context.Context, string, string, bool, time.Duration) error   { return nil }
func (f *urlFake) Fill(context.Context, string, string, string) error                 { return nil }
func (f *urlFake) CurrentURL(context.Context) (string, error)                         { return "", nil }
func (f *urlFake) Close()                                                             {}

const accountsJSON = `[{"accountReferenceId":"REF1","last4":"3040","productName":"Venture X"}]`

const feedJSON = `{"cursor":"","data":[
 {"merchantTLD":"expedia.com","merchantDisplayName":"Expedia","type":"Standard","id":"ID1","buttonText":"Up to 5X miles","text":"Online"},
 {"merchantTLD":"chewy.com","merchantDisplayName":"Chewy","type":"Standard","id":"ID2","buttonText":"7X miles","text":"Online"}]}`

// trimmed real capture from /xhr/feed/{ref}/offers/{id}
const expediaDetailJSON = `{"success":true,"offer":{
 "rewardDisplayText":"Up to 5X miles",
 "termsAndConditions":[
   {"title":"Merchant details","markdownTextSections":["To earn the rebate you must make an eligible purchase at expedia.com. **Use of promotional codes** not provided may invalidate your offer."]},
   {"title":"How to receive the payout","markdownTextSections":["We will attempt to issue the payout to your Venture X account.","Payouts are usually issued within [45 days](https://x.y/t)."]}],
 "disclaimer":"Payouts are usually issued within 45 days after a completed stay.",
 "affiliate":{"header":"Shop Online","displayAmount":"Up to 5X miles",
   "categories":[{"displayAmount":"5X miles","name":"Completed Hotel Stay"},{"displayAmount":"3X miles","name":"Completed Car Rental"}],
   "callouts":[{"type":"exclusion","markdownText":"**Review exclusions:** Avoid ad blockers."}]}}}`

func TestDetailEnrichment(t *testing.T) {
	fake := &urlFake{pages: map[string]string{
		"/user-accounts": accountsJSON,
		"/feed/":         feedJSON,
		"/xhr/feed/":     expediaDetailJSON, // only Expedia fetch matches substring; Chewy gets same doc (fine)
	}}
	bank := &config.Bank{
		ID:      "capitalone",
		BaseURL: "https://capitaloneoffers.com",
		RateLimit: config.RateLimit{
			RequestsPerRun: 50,
			DelayBetween:   config.Duration(time.Millisecond),
		},
		Offers: []config.Step{{FetchAPI: &config.FetchAPISpec{
			Name: "feed",
			URL:  "/feed/{{urlquery .acct.accountReferenceId}}?contentSlug=ease-web-l1",
			Each: &config.EachSpec{From: "accounts", Items: "$[*]", As: "acct", CardLabel: "{{.productName}} •{{.last4}}"},
			Detail: &config.DetailSpec{
				URL: "/xhr/feed/{{urlquery .acct.accountReferenceId}}/offers/{{urlquery .offer.sourceId}}",
				Notes: []config.NoteBlock{
					{Root: "offer.termsAndConditions", PrefixFrom: "title", TextFrom: "markdownTextSections"},
					{Root: "offer.affiliate.categories", PrefixFrom: "name", TextFrom: "displayAmount"},
					{Root: "offer.affiliate.callouts", TextFrom: "markdownText"},
				},
			},
			Extract: &config.Extract{
				Items:  "data[*]",
				Status: "eligible",
				Fields: map[string]config.FieldMap{
					"merchant": {Path: "merchantDisplayName"},
					"title":    {Path: "buttonText"},
					"sourceId": {Path: "id"},
					"channel":  {Path: "text"},
				},
			},
		}},
		},
	}
	r := &Runner{Bank: bank, Browser: fake, Log: nil, Now: func() time.Time { return time.Unix(0, 0).UTC() }}

	// account step first (mimics the real two-step config)
	r.Bank.Offers = append([]config.Step{{FetchAPI: &config.FetchAPISpec{
		Name: "accounts", URL: "/user-accounts", SaveAs: "accounts",
	}}}, r.Bank.Offers...)

	res := r.Run(context.Background())
	if res.Err != nil {
		t.Fatalf("run err: %v (skipped: %v)", res.Err, res.SkippedLog)
	}
	if len(res.Offers) != 2 {
		t.Fatalf("offers=%d want 2", len(res.Offers))
	}
	for _, o := range res.Offers {
		if o.CardLabel != "Venture X •3040" {
			t.Errorf("cardLabel=%q (each.cardLabel not applied)", o.CardLabel)
		}
	}
	var expedia *offerT
	for i := range res.Offers {
		if res.Offers[i].Merchant == "Expedia" {
			expedia = &offerT{res.Offers[i].Notes}
		}
	}
	if expedia == nil {
		t.Fatal("no Expedia offer")
	}
	for _, want := range []string{
		"Merchant details: To earn the rebate",      // title + sections joined
		"Use of promotional codes",                  // ** stripped
		"45 days (https://x.y/t)",                   // md link → text (url)
		"Completed Hotel Stay: 5X miles",            // category rates
		"Review exclusions: Avoid ad blockers",      // callouts
		"Payouts are usually issued within 45 days", // disclaimer? not configured -> must be absent
	} {
		got := strings.Contains(expedia.notes, want)
		if strings.HasPrefix(want, "Payouts") {
			// disclaimer-specific tail must not leak (text came from terms section)
			if strings.Contains(expedia.notes, "after a completed stay") {
				t.Errorf("disclaimer leaked into notes: %q", expedia.notes)
			}
			continue
		}
		if !got {
			t.Errorf("notes missing %q\n---\n%s", want, expedia.notes)
		}
	}
	// detail URL must carry the escaped per-offer id
	found := false
	for _, c := range fake.calls {
		if strings.Contains(c, "/xhr/feed/REF1/offers/ID1") {
			found = true
		}
	}
	if !found {
		for _, c := range fake.calls {
			if strings.Contains(c, "fetch(") {
				m := regexp.MustCompile(`fetch\((?:"|` + "`" + `)([^"]{0,120})`).FindString(c)
				t.Logf("call: %s", m)
			}
		}
		t.Errorf("detail url not rendered with .offer.id; calls=%d", len(fake.calls))
	}
}

type offerT struct{ notes string }
