package web

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rajkumaar23/offerhound/internal/store"
)

func newTestStore(t *testing.T) *store.Store {
	t.Helper()
	s, err := store.Open(filepath.Join(t.TempDir(), "web.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func post(t *testing.T, url, token, body string) *http.Response {
	t.Helper()
	req, _ := http.NewRequest("POST", url, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("X-Offerhound-Token", token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

const ingestBody = `{"user":"amy","bank":"c1","complete":true,"offers":[
  {"id":"a","merchant":"Chewy","rewardText":"Up to 7X miles","channel":"Online",
   "status":"eligible","notes":"7X on Autoship only"}]}`

func TestIngestAuthAndSearch(t *testing.T) {
	s := newTestStore(t)
	hs := httptest.NewServer(New(s, "tok123").Handler())
	defer hs.Close()

	// POST without token => 401 (writes are gated)
	if resp := post(t, hs.URL+"/api/ingest", "", ingestBody); resp.StatusCode != 401 {
		t.Fatalf("want 401, got %d", resp.StatusCode)
	}
	resp := post(t, hs.URL+"/api/ingest", "tok123", ingestBody)
	if resp.StatusCode != 200 {
		t.Fatalf("ingest with token: %d", resp.StatusCode)
	}
	resp.Body.Close()

	// GETs are OPEN — no token anywhere below
	resp, _ = http.Get(hs.URL + "/api/search?user=amy&q=autoship")
	var offers []map[string]any
	json.NewDecoder(resp.Body).Decode(&offers)
	resp.Body.Close()
	if len(offers) != 1 || offers[0]["merchant"] != "Chewy" {
		t.Fatalf("notes search over open API: %+v", offers)
	}
	// popup payload must carry the fine print
	if offers[0]["notes"] != "7X on Autoship only" {
		t.Fatalf("notes not served: %+v", offers[0])
	}
	// another wallet sees nothing
	resp, _ = http.Get(hs.URL + "/api/search?user=bob")
	var none []map[string]any
	json.NewDecoder(resp.Body).Decode(&none)
	resp.Body.Close()
	if len(none) != 0 {
		t.Fatalf("wallet leak: %+v", none)
	}

	// UI serves and links assets
	resp, _ = http.Get(hs.URL + "/")
	if resp.StatusCode != 200 || !strings.Contains(mustRead(resp), "offerhound") {
		t.Fatal("index page")
	}
	resp, _ = http.Get(hs.URL + "/assets/app.js")
	if resp.StatusCode != 200 {
		t.Fatal("app.js asset")
	}
}

func TestUsersDropdown(t *testing.T) {
	s := newTestStore(t)
	hs := httptest.NewServer(New(s, "tok123").Handler())
	defer hs.Close()
	post(t, hs.URL+"/api/ingest", "tok123", ingestBody).Body.Close()
	post(t, hs.URL+"/api/ingest", "tok123",
		`{"user":"bob","bank":"examplebank","complete":true,"offers":[
		  {"id":"z","merchant":"Target","rewardText":"5%","status":"eligible"}]}`).Body.Close()

	resp, _ := http.Get(hs.URL + "/api/users")
	var users []string
	json.NewDecoder(resp.Body).Decode(&users)
	resp.Body.Close()
	if len(users) != 2 || users[0] != "amy" || users[1] != "bob" {
		t.Fatalf("users dropdown: %v", users)
	}
}

func TestHashOnlyServer(t *testing.T) {
	s := newTestStore(t)
	// server is configured with ONLY the hash — plaintext secret lives on clients
	hs := httptest.NewServer(NewWithHash(s, store.TokenHash("sekrit")).Handler())
	defer hs.Close()
	if resp := post(t, hs.URL+"/api/ingest", "wrong", ingestBody); resp.StatusCode != 401 {
		t.Fatalf("wrong token: %d", resp.StatusCode)
	}
	if resp := post(t, hs.URL+"/api/ingest", "sekrit", ingestBody); resp.StatusCode != 200 {
		t.Fatalf("hashed-token server must accept the plaintext token: %d", resp.StatusCode)
	}
}

func TestHumanPromptNoScreenshots(t *testing.T) {
	s := newTestStore(t)
	hs := httptest.NewServer(New(s, "tok123").Handler())
	defer hs.Close()

	// a sync client asks a human via the server (token-gated write)
	resp := post(t, hs.URL+"/api/human", "tok123",
		`{"user":"amy","bank":"c1","prompt":"Enter the SMS code"}`)
	if resp.StatusCode != 200 {
		t.Fatalf("ask: %d", resp.StatusCode)
	}
	var asked struct {
		ID int64 `json:"id"`
	}
	json.NewDecoder(resp.Body).Decode(&asked)
	resp.Body.Close()
	if asked.ID == 0 {
		t.Fatal("no request id")
	}

	// the waiting client fetches answers — this read carries a code, so it is gated
	req, _ := http.NewRequest("GET", hs.URL+"/api/human/answers?waiting="+itoa(asked.ID), nil)
	resp, _ = http.DefaultClient.Do(req)
	if resp.StatusCode != 401 {
		t.Fatalf("answer delivery must be token-gated, got %d", resp.StatusCode)
	}
	resp.Body.Close()

	// the human on the phone has no token — answer post stays open
	if resp := post(t, hs.URL+"/api/human/"+itoa(asked.ID)+"/answer", "",
		`{"answer":"424242"}`); resp.StatusCode != 200 {
		t.Fatalf("open answer post: %d", resp.StatusCode)
	}

	req, _ = http.NewRequest("GET", hs.URL+"/api/human/answers?waiting="+itoa(asked.ID), nil)
	req.Header.Set("X-Offerhound-Token", "tok123")
	resp, _ = http.DefaultClient.Do(req)
	var answers []map[string]any
	json.NewDecoder(resp.Body).Decode(&answers)
	resp.Body.Close()
	if len(answers) != 1 || answers[0]["answer"] != "424242" {
		t.Fatalf("answer delivery: %+v", answers)
	}
}

func itoa(i int64) string { return strings.TrimSpace(jsonNum(i)) }

func jsonNum(i int64) string {
	b, _ := json.Marshal(i)
	return string(b)
}

func mustRead(resp *http.Response) string {
	b, _ := io.ReadAll(resp.Body)
	return string(b)
}
