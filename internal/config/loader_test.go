package config

import (
	"strings"
	"testing"
)

const base = `
id: acme
name: Acme Card
baseUrl: https://offers.acme.example
credentials:
  username: env://OH_ACME_USER
  password: env://OH_ACME_PASS
login:
  probe:
    fetchApi: /whoami
    okStatus: [200]
  steps:
    - navigate: https://offers.acme.example/login
    - waitSelector: { selector: "#u", timeout: 10s }
    - fill: { selector: "#u", valueFrom: credential.username }
    - fill: { selector: "#p", valueFrom: credential.password }
    - click: { selector: "button.go", trusted: true }
offers:
  - name: list
    fetchApi: /api/offers
    saveAs: raw
    extract:
      items: "$.data[*]"
      fields:
        merchant: name
        rewardText: reward
`

func TestParseValid(t *testing.T) {
	b, err := Parse([]byte(base), "test.yaml")
	if err != nil {
		t.Fatalf("valid config rejected: %v", err)
	}
	if b.ID != "acme" || len(b.Offers) != 1 || b.Offers[0].FetchAPI == nil {
		t.Fatalf("unexpected parse: %+v", b)
	}
}

func TestClickForbiddenInOffers(t *testing.T) {
	bad := base + "  - click: { selector: \".add\" }\n"
	_, err := Parse([]byte(bad), "bad.yaml")
	if err == nil || !strings.Contains(err.Error(), "read-only rule") {
		t.Fatalf("expected read-only rejection, got %v", err)
	}
}

func TestLiteralCredentialRejected(t *testing.T) {
	bad := strings.Replace(base, "env://OH_ACME_PASS", "hunter2", 1)
	_, err := Parse([]byte(bad), "bad.yaml")
	if err == nil || !strings.Contains(err.Error(), "not a literal") {
		t.Fatalf("expected literal credential rejection, got %v", err)
	}
}

func TestUnknownKeyRejected(t *testing.T) {
	bad := base + "\nextraField: 7\n"
	if _, err := Parse([]byte(bad), "bad.yaml"); err == nil {
		t.Fatal("expected unknown-field error")
	}
}

func TestWaitNeedsTimeout(t *testing.T) {
	bad := strings.Replace(base, "waitSelector: { selector: \"#u\", timeout: 10s }",
		"waitSelector: { selector: \"#u\" }", 1)
	if _, err := Parse([]byte(bad), "bad.yaml"); err == nil {
		t.Fatal("expected timeout-required error")
	}
}

func TestEachFromUnknown(t *testing.T) {
	bad := strings.Replace(base, "saveAs: raw", "saveAs: other", 1) + `
  - name: looped
    each: { from: raw, items: "$[*]", as: a }
    fetchApi: "/x/{{urlquery .a.id}}"
    extract: { items: "$.data[*]", fields: { merchant: m } }
`
	_, err := Parse([]byte(bad), "bad.yaml")
	if err == nil || !strings.Contains(err.Error(), "not saved") {
		t.Fatalf("expected each.from validation, got %v", err)
	}
}

func TestAttachModeRequiresCDP(t *testing.T) {
	bad := strings.Replace(base, "baseUrl: https://offers.acme.example",
		"baseUrl: https://offers.acme.example\nbrowser:\n  mode: attach", 1)
	if _, err := Parse([]byte(bad), "bad.yaml"); err == nil {
		t.Fatal("expected cdpUrl requirement")
	}
}

func TestExamplesDir(t *testing.T) {
	banks, err := LoadDir("../../docs/examples")
	if err != nil {
		t.Fatalf("examples must validate: %v", err)
	}
	if len(banks) < 1 {
		t.Fatalf("expected >=1 example config, got %d", len(banks))
	}
}
