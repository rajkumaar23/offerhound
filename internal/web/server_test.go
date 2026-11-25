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

func newTestServer(t *testing.T) (*httptest.Server, *store.Store) {
	t.Helper()
	s, err := store.Open(filepath.Join(t.TempDir(), "web.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	sv := New(s, "tok123")
	hs := httptest.NewServer(sv.Handler())
	t.Cleanup(hs.Close)
	return hs, s
}

func TestIngestAuthAndSearch(t *testing.T) {
	hs, _ := newTestServer(t)
	body := `{"bank":"c1","complete":true,"offers":[
	  {"id":"a","merchant":"Chewy","rewardText":"Up to 7X miles","channel":"Online",
	   "status":"eligible","notes":"7X on Autoship only"}]}`

	// no token => 401
	req, _ := http.NewRequest("POST", hs.URL+"/api/ingest", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != 401 {
		t.Fatalf("want 401, got %d", resp.StatusCode)
	}
	resp.Body.Close()

	// right token => ok
	req, _ = http.NewRequest("POST", hs.URL+"/api/ingest", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Offerhound-Token", "tok123")
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != 200 {
		t.Fatalf("ingest with token: %d", resp.StatusCode)
	}
	resp.Body.Close()

	// notes are searchable
	resp, _ = http.Get(hs.URL + "/api/search?q=autoship")
	var offers []map[string]any
	json.NewDecoder(resp.Body).Decode(&offers)
	resp.Body.Close()
	if len(offers) != 1 || offers[0]["merchant"] != "Chewy" {
		t.Fatalf("notes search over API: %+v", offers)
	}
	// popup payload must carry the fine print
	if offers[0]["notes"] != "7X on Autoship only" {
		t.Fatalf("notes not served: %+v", offers[0])
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

func TestSyncEnqueueAndPoll(t *testing.T) {
	hs, st := newTestServer(t)
	req, _ := http.NewRequest("POST", hs.URL+"/api/sync", strings.NewReader(`{"bank":"c1"}`))
	req.Header.Set("X-Offerhound-Token", "tok123")
	resp, err := http.DefaultClient.Do(req)
	if err != nil || resp.StatusCode != 200 {
		t.Fatalf("enqueue: %v %d", err, resp.StatusCode)
	}
	resp.Body.Close()
	// worker polls jobs
	req, _ = http.NewRequest("GET", hs.URL+"/api/jobs?worker=w1&banks=c1", nil)
	req.Header.Set("X-Offerhound-Token", "tok123")
	resp, _ = http.DefaultClient.Do(req)
	var poll struct {
		Job *struct {
			ID   int64  `json:"id"`
			Bank string `json:"bank"`
		} `json:"job"`
	}
	json.NewDecoder(resp.Body).Decode(&poll)
	resp.Body.Close()
	if poll.Job == nil || poll.Job.Bank != "c1" {
		t.Fatalf("job not delivered: %+v", poll)
	}
	_ = st
}

func mustRead(resp *http.Response) string {
	b, _ := io.ReadAll(resp.Body)
	return string(b)
}
