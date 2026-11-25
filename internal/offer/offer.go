package offer

import "time"

// Status of an offer relative to the user's card.
type Status string

const (
	Eligible Status = "eligible" // available to add
	Enrolled Status = "enrolled" // already added to card
)

// Offer is a normalized, bank-agnostic card offer.
type Offer struct {
	ID          string    `json:"id"`
	Bank        string    `json:"bank"`
	CardLabel   string    `json:"cardLabel"`
	Merchant    string    `json:"merchant"`
	Title       string    `json:"title"`
	Description string    `json:"description,omitempty"`
	RewardText  string    `json:"rewardText"`
	RewardValue float64   `json:"rewardValue,omitempty"` // parsed magnitude (10 for "10%", 5 for "$5")
	Channel     string    `json:"channel,omitempty"`     // Online | In-Store | …
	Status      Status    `json:"status"`
	Notes       string    `json:"notes,omitempty"` // fine print: category rates, exclusions
	TermsURL    string    `json:"termsUrl,omitempty"`
	ExpiryDate  string    `json:"expiryDate,omitempty"` // YYYY-MM-DD
	Source      string    `json:"source,omitempty"`
	DetailURL   string    `json:"detailUrl,omitempty"`
	ScrapedAt   time.Time `json:"scrapedAt"`
}

// Card is issuer-account metadata shown as chips in the UI.
type Card struct {
	Label string            `json:"label"`
	Extra map[string]string `json:"extra,omitempty"`
}

// Dedupe collapses offers by ID; enrolled wins over eligible.
func Dedupe(in []Offer) []Offer {
	byID := make(map[string]Offer, len(in))
	order := make([]string, 0, len(in))
	for _, o := range in {
		prev, seen := byID[o.ID]
		if !seen {
			byID[o.ID] = o
			order = append(order, o.ID)
			continue
		}
		if prev.Status != Enrolled && o.Status == Enrolled {
			byID[o.ID] = o
		}
	}
	out := make([]Offer, 0, len(order))
	for _, id := range order {
		out = append(out, byID[id])
	}
	return out
}
