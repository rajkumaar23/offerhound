package engine

import (
	"context"
	"strings"
	"testing"
)

// TestFetchExpression pins the generated RequestInit: bank pages wrap
// window.fetch (Datadog RUM on capitaloneoffers.com) and their validation
// throws TypeError on null headers. A rebase once silently reverted the
// "{}"-not-null fix; unit tests saw nothing because the fake ignored the
// expression. This test makes that regression impossible.
func TestFetchExpression(t *testing.T) {
	cases := []struct {
		name    string
		headers map[string]string
		want    string
		deny    string
	}{
		{"nil headers send empty record", nil, "headers: {}", "null"},
		{"empty map sends empty record", map[string]string{}, "headers: {}", "null"},
		{"real headers preserved", map[string]string{"Accept": "application/json"},
			`headers: {"Accept":"application/json"}`, "null"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fb := &fakeBrowser{body: `[]`, status: 200}
			r := &Runner{Browser: fb}
			if _, _, _, err := r.fetch(context.Background(), "https://example.test/api", tc.headers); err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(fb.lastExpr, tc.want) {
				t.Fatalf("expr missing %q:\n%s", tc.want, fb.lastExpr)
			}
			if strings.Contains(fb.lastExpr, tc.deny) {
				t.Fatalf("expr must never contain %q:\n%s", tc.deny, fb.lastExpr)
			}
		})
	}
}
