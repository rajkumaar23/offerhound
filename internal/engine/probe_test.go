package engine

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/rajkumaar23/offerhound/internal/config"
)

// fakeBrowser answers only what probe() exercises: EvaluateJSON returns the
// canned fetch envelope.
type fakeBrowser struct {
	body   string
	status int
	err    error
}

func (f *fakeBrowser) Navigate(context.Context, string) error { return nil }
func (f *fakeBrowser) EvaluateJSON(context.Context, string) (string, error) {
	if f.err != nil {
		return "", f.err
	}
	// Mirror the real contract: the JS side JSON.stringify's the envelope
	// and body is the raw response *string*.
	env, _ := json.Marshal(struct {
		Status     int    `json:"status"`
		RetryAfter string `json:"retryAfter"`
		Body       string `json:"body"`
	}{f.status, "", f.body})
	return string(env), nil
}
func (f *fakeBrowser) WaitFor(context.Context, string, string, bool, time.Duration) error {
	return nil
}
func (f *fakeBrowser) Click(context.Context, string, string, bool, time.Duration) error {
	return nil
}
func (f *fakeBrowser) Fill(context.Context, string, string, string) error { return nil }
func (f *fakeBrowser) CurrentURL(context.Context) (string, error)         { return "", nil }
func (f *fakeBrowser) Close()                                             {}

func TestProbeHasPath(t *testing.T) {
	cases := []struct {
		name    string
		body    string
		status  int
		hasPath string
		want    bool
	}{
		{"array present", `[{"accountReferenceId":"a1"}]`, 200, "0.accountReferenceId", true},
		{"200 html shell is signed out", `<!DOCTYPE html><html>`, 200, "0.accountReferenceId", false},
		{"200 json error object is signed out", `{"error":"unauthorized"}`, 200, "0.accountReferenceId", false},
		{"200 explicit null is signed out", `null`, 200, "0.accountReferenceId", false},
		{"200 empty array fails path", `[]`, 200, "0.accountReferenceId", false},
		{"status mismatch wins", `[{"accountReferenceId":"a1"}]`, 401, "0.accountReferenceId", false},
		{"no hasPath keeps status-only", `anything`, 200, "", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := &Runner{
				Bank: &config.Bank{
					ID:      "t",
					BaseURL: "https://example.test",
					Login: &config.Login{Probe: &config.Probe{
						FetchAPI: "/user-accounts",
						OKStatus: []int{200},
						HasPath:  tc.hasPath,
					}},
				},
				Browser: &fakeBrowser{body: tc.body, status: tc.status},
			}
			got, err := r.probe(context.Background())
			if err != nil {
				t.Fatalf("probe err: %v", err)
			}
			if got != tc.want {
				t.Fatalf("probe = %v, want %v", got, tc.want)
			}
		})
	}
}
