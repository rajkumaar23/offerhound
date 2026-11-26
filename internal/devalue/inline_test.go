package devalue

import (
	"encoding/json"
	"testing"
)

func TestDecodeInline(t *testing.T) {
	// Mirrors the wire shapes measured on the American Express offers
	// hydration blob 2026-11-26: everything inline, four marker kinds.
	raw := `[
	  "~#iM",
	  [
	    "result", ["~#iM", [
	      "addedToCard", ["~#iM", [
	        "offersList", ["~#iM", [
	          "page1", ["~#iL", [
	            ["^ ", "offerId", "abc", "title", "Citizen Watch",
	             "enrollmentDetails", ["^ ", "status", "ENROLLED"],
	             "applicableFilters", ["~#iL", [
	               ["^ ", "text", "Online", "optionType", "ONLINE"]]]]
	          ]]
	        ]]
	      ]],
	      "flags", ["~#iS", ["a", "b", "c"]],
	      "count", 100,
	      "ok", true
	    ]]
	  ]
	]`
	got, err := DecodeInline([]byte(raw))
	if err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(got)
	var doc struct {
		Result struct {
			AddedToCard struct {
				OffersList struct {
					Page1 []struct {
						OfferID           string `json:"offerId"`
						Title             string `json:"title"`
						EnrollmentDetails struct {
							Status string `json:"status"`
						} `json:"enrollmentDetails"`
						ApplicableFilters []struct {
							Text string `json:"text"`
						} `json:"applicableFilters"`
					} `json:"page1"`
				} `json:"offersList"`
			} `json:"addedToCard"`
			Flags []string `json:"flags"`
			Count int      `json:"count"`
			OK    bool     `json:"ok"`
		} `json:"result"`
	}
	if err := json.Unmarshal(b, &doc); err != nil {
		t.Fatalf("re-marshal produced unexpected shape: %v\n%s", err, b)
	}
	r := doc.Result
	if len(r.AddedToCard.OffersList.Page1) != 1 {
		t.Fatalf("page1: %+v", r.AddedToCard.OffersList.Page1)
	}
	o := r.AddedToCard.OffersList.Page1[0]
	if o.OfferID != "abc" || o.Title != "Citizen Watch" || o.EnrollmentDetails.Status != "ENROLLED" {
		t.Fatalf("offer: %+v", o)
	}
	if len(o.ApplicableFilters) != 1 || o.ApplicableFilters[0].Text != "Online" {
		t.Fatalf("filters: %+v", o.ApplicableFilters)
	}
	if len(r.Flags) != 3 || r.Flags[0] != "a" || r.Count != 100 || !r.OK {
		t.Fatalf("scalars/set: %+v", r)
	}
}

func TestDecodeInlineUnknownMarkerIsList(t *testing.T) {
	got, err := DecodeInline([]byte(`["~#iZ", 1, 2]`))
	if err != nil {
		t.Fatal(err)
	}
	list, ok := got.([]any)
	if !ok || len(list) != 2 || list[0].(float64) != 1 {
		t.Fatalf("got %#v", got)
	}
}

func TestDecodeInlineRejectsNonJSON(t *testing.T) {
	if _, err := DecodeInline([]byte("<html>")); err == nil {
		t.Fatal("expected error")
	}
}
