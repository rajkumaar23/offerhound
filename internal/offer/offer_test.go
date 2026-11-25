package offer

import "testing"

func TestDedupeEnrolledWins(t *testing.T) {
	in := []Offer{
		{ID: "a", Merchant: "Amazon", Status: Eligible},
		{ID: "a", Merchant: "Amazon", Status: Enrolled},
		{ID: "b", Merchant: "Target", Status: Eligible},
	}
	out := Dedupe(in)
	if len(out) != 2 {
		t.Fatalf("want 2, got %d", len(out))
	}
	if out[0].ID != "a" || out[0].Status != Enrolled {
		t.Fatalf("enrolled should win: %+v", out[0])
	}
}

func TestDedupeKeepsFirstEnrolled(t *testing.T) {
	in := []Offer{
		{ID: "a", Status: Enrolled, Notes: "first"},
		{ID: "a", Status: Eligible, Notes: "second"},
	}
	out := Dedupe(in)
	if out[0].Notes != "first" {
		t.Fatalf("first enrolled should stick, got %q", out[0].Notes)
	}
}
