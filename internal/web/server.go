// Package web serves the offerhound UI and JSON API.
package web

import (
	"embed"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/rajkumaar23/offerhound/internal/engine"
	"github.com/rajkumaar23/offerhound/internal/offer"
	"github.com/rajkumaar23/offerhound/internal/store"
)

var bankIDRe = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,31}$`)

//go:embed templates/* assets/*
var static embed.FS

// Server hosts the UI + API over a Store.
type Server struct {
	Store *store.Store
	Token string // required on /api writes (and all routes off-loopback)
}

// New builds a server.
func New(s *store.Store, token string) *Server {
	return &Server{Store: s, Token: token}
}

// Handler returns the routed handler (usable in httptest).
func (sv *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", sv.pageIndex)
	mux.HandleFunc("GET /assets/{name}", sv.serveAsset)
	mux.HandleFunc("GET /api/search", sv.authRead(sv.apiSearch))
	mux.HandleFunc("GET /api/banks", sv.authRead(sv.apiBanks))
	mux.HandleFunc("GET /api/human", sv.authRead(sv.apiHumanPending))
	mux.HandleFunc("POST /api/human/{id}/answer", sv.authWrite(sv.apiHumanAnswer))
	mux.HandleFunc("POST /api/sync", sv.authWrite(sv.apiSync))
	// collector-side endpoints
	mux.HandleFunc("POST /api/ingest", sv.authWrite(sv.apiIngest))
	mux.HandleFunc("POST /api/runs", sv.authWrite(sv.apiStartRun))
	mux.HandleFunc("POST /api/runs/{id}/finish", sv.authWrite(sv.apiFinishRun))
	mux.HandleFunc("GET /api/jobs", sv.authWrite(sv.apiJobsPoll))
	mux.HandleFunc("POST /api/jobs/{id}/complete", sv.authWrite(sv.apiJobComplete))
	mux.HandleFunc("POST /api/human", sv.authWrite(sv.apiAskHuman))
	return mux
}

// --- auth ----------------------------------------------------------------

func (sv *Server) authed(r *http.Request) bool {
	if r.Header.Get("X-Offerhound-Token") == sv.Token {
		return true
	}
	host, _, _ := net.SplitHostPort(r.RemoteAddr)
	return host == "127.0.0.1" || host == "::1"
}

func (sv *Server) authRead(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !sv.authed(r) {
			http.Error(w, `{"error":"unauthorized"}`, http.StatusUnauthorized)
			return
		}
		next(w, r)
	}
}

func (sv *Server) authWrite(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if sv.Token != "" && r.Header.Get("X-Offerhound-Token") != sv.Token {
			http.Error(w, `{"error":"unauthorized"}`, http.StatusUnauthorized)
			return
		}
		if sv.Token == "" && !sv.authed(r) {
			http.Error(w, `{"error":"unauthorized"}`, http.StatusUnauthorized)
			return
		}
		next(w, r)
	}
}

// --- pages -----------------------------------------------------------------

func (sv *Server) pageIndex(w http.ResponseWriter, r *http.Request) {
	tpl, err := static.ReadFile("templates/index.html")
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Write(tpl)
}

func (sv *Server) serveAsset(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if strings.Contains(name, "..") {
		http.NotFound(w, r)
		return
	}
	b, err := static.ReadFile("assets/" + name)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	if strings.HasSuffix(name, ".css") {
		w.Header().Set("Content-Type", "text/css")
	} else if strings.HasSuffix(name, ".js") {
		w.Header().Set("Content-Type", "text/javascript")
	}
	w.Write(b)
}

// --- api -------------------------------------------------------------------

func jsonOut(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

func decode(w http.ResponseWriter, r *http.Request, v any) bool {
	if err := json.NewDecoder(r.Body).Decode(v); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return false
	}
	return true
}

func (sv *Server) apiSearch(w http.ResponseWriter, r *http.Request) {
	qs := r.URL.Query()
	res, err := sv.Store.Search(store.Query{
		Text:    qs.Get("q"),
		Bank:    qs.Get("bank"),
		Status:  qs.Get("status"),
		Channel: qs.Get("channel"),
		Limit:   atoiDefault(qs.Get("limit"), 200),
	})
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	if res == nil {
		res = []offer.Offer{}
	}
	jsonOut(w, res)
}

func (sv *Server) apiBanks(w http.ResponseWriter, r *http.Request) {
	rows, err := sv.Store.Banks()
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	if rows == nil {
		rows = []store.BankRow{}
	}
	jsonOut(w, rows)
}

func (sv *Server) apiIngest(w http.ResponseWriter, r *http.Request) {
	var req engine.IngestReq
	if !decode(w, r, &req) {
		return
	}
	if req.Bank == "" || !bankIDRe.MatchString(req.Bank) {
		http.Error(w, "bad bank id", http.StatusBadRequest)
		return
	}
	if err := sv.Store.EnsureBank(req.Bank, req.Bank); err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	err := sv.Store.Ingest(req.Bank, req.Cards, req.Offers, req.Complete, req.Note)
	if errors.Is(err, store.ErrPartialRejected) {
		http.Error(w, err.Error(), http.StatusConflict)
		return
	}
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	jsonOut(w, map[string]any{"ok": true, "ingested": len(req.Offers)})
}

func (sv *Server) apiStartRun(w http.ResponseWriter, r *http.Request) {
	var body struct{ Bank, Worker string }
	if !decode(w, r, &body) {
		return
	}
	if err := sv.Store.EnsureBank(body.Bank, body.Bank); err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	id, err := sv.Store.StartRun(body.Bank, body.Worker)
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	jsonOut(w, map[string]any{"runId": id})
}

func (sv *Server) apiFinishRun(w http.ResponseWriter, r *http.Request) {
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	var body struct {
		Status     string `json:"status"`
		Error      string `json:"error"`
		OfferCount int    `json:"offerCount"`
	}
	if !decode(w, r, &body) {
		return
	}
	if err := sv.Store.FinishRun(id, body.Status, body.Error, body.OfferCount); err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	jsonOut(w, map[string]any{"ok": true})
}

func (sv *Server) apiJobsPoll(w http.ResponseWriter, r *http.Request) {
	qs := r.URL.Query()
	worker := qs.Get("worker")
	var banks []string
	for _, b := range qs["banks"] {
		if bankIDRe.MatchString(b) {
			banks = append(banks, b)
		}
	}
	var waiting []int64
	for _, s := range qs["waiting"] {
		if id, err := strconv.ParseInt(s, 10, 64); err == nil {
			waiting = append(waiting, id)
		}
	}
	answers, err := sv.Store.AnswersFor(waiting)
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	job, err := sv.Store.ClaimJob(worker, banks)
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	jsonOut(w, map[string]any{"job": job, "answers": answers})
}

func (sv *Server) apiJobComplete(w http.ResponseWriter, r *http.Request) {
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	var body struct{ Status, Error string }
	if !decode(w, r, &body) {
		return
	}
	if err := sv.Store.CompleteJob(id, body.Status, body.Error); err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	jsonOut(w, map[string]any{"ok": true})
}

func (sv *Server) apiAskHuman(w http.ResponseWriter, r *http.Request) {
	var body struct {
		RunID         int64  `json:"runId"`
		Bank          string `json:"bank"`
		Prompt        string `json:"prompt"`
		ScreenshotB64 string `json:"screenshotB64"`
	}
	if !decode(w, r, &body) {
		return
	}
	var shot []byte
	if body.ScreenshotB64 != "" {
		var err error
		shot, err = base64.StdEncoding.DecodeString(body.ScreenshotB64)
		if err != nil {
			http.Error(w, "bad screenshot", http.StatusBadRequest)
			return
		}
	}
	id, err := sv.Store.AskHuman(body.RunID, body.Bank, body.Prompt, shot)
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	jsonOut(w, map[string]any{"id": id})
}

func (sv *Server) apiHumanPending(w http.ResponseWriter, r *http.Request) {
	rows, err := sv.Store.PendingHuman()
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	if rows == nil {
		rows = []store.HumanRequest{}
	}
	jsonOut(w, rows)
}

func (sv *Server) apiHumanAnswer(w http.ResponseWriter, r *http.Request) {
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	var body struct{ Answer string }
	if !decode(w, r, &body) {
		return
	}
	ok, err := sv.Store.AnswerHuman(id, body.Answer)
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	if !ok {
		http.Error(w, "already resolved", http.StatusGone)
		return
	}
	jsonOut(w, map[string]any{"ok": true})
}

func (sv *Server) apiSync(w http.ResponseWriter, r *http.Request) {
	var body struct{ Bank string }
	if !decode(w, r, &body) {
		return
	}
	if !bankIDRe.MatchString(body.Bank) {
		http.Error(w, "bad bank id", http.StatusBadRequest)
		return
	}
	id, err := sv.Store.EnqueueJob(body.Bank)
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	jsonOut(w, map[string]any{"queued": id})
}

func atoiDefault(s string, def int) int {
	if n, err := strconv.Atoi(s); err == nil && n > 0 {
		return n
	}
	return def
}

// Serve is a convenience blocking server.
func (sv *Server) Serve(listen string) error {
	srv := &http.Server{
		Addr:              listen,
		Handler:           sv.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
	}
	return srv.ListenAndServe()
}
