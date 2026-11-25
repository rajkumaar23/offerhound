// Package web serves the offerhound UI (single page, phone-first) and the
// JSON API.
//
// The server is deliberately dumb: it stores what clients push and renders
// it. No scheduler, no job queue, no run bookkeeping — clients (cron,
// manual `offerhound sync`, CI) decide when to send data.
//
// Auth model:
//   - GETs are OPEN by design. Collected card offers mirror public
//     marketing data; the `user` selector is a scoping key, not a secret.
//     The only read exception is answer delivery for human prompts, which
//     carries OTP codes and is token-gated.
//   - POSTs require X-Offerhound-Token whose SHA-256 hash matches the
//     configured hash (constant-time compare). Clients hold the plaintext
//     token; the server can run on only the hash (--token-hash) so a
//     server-side leak never exposes the token.
//   - No screenshots or page artifacts ever pass through this layer.
package web

import (
	"embed"
	"encoding/json"
	"errors"
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
	Store     *store.Store
	tokenHash string // hex SHA-256; empty => writes open (single-user LAN)
}

// New builds a server from a plaintext token (hashed immediately; the
// plaintext is not retained).
func New(s *store.Store, token string) *Server {
	hash := ""
	if token != "" {
		hash = store.TokenHash(token)
	}
	return &Server{Store: s, tokenHash: hash}
}

// NewWithHash builds a server from a pre-computed hex SHA-256 token hash,
// so the plaintext token never touches the server's config or memory.
func NewWithHash(s *store.Store, tokenHash string) *Server {
	return &Server{Store: s, tokenHash: strings.TrimSpace(tokenHash)}
}

// Handler returns the routed handler (usable in httptest).
func (sv *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	// reads: open (offer data is not private info)
	mux.HandleFunc("GET /{$}", sv.pageIndex)
	mux.HandleFunc("GET /assets/{name}", sv.serveAsset)
	mux.HandleFunc("GET /api/users", sv.apiUsers)
	mux.HandleFunc("GET /api/search", sv.apiSearch)
	mux.HandleFunc("GET /api/banks", sv.apiBanks)
	mux.HandleFunc("GET /api/human", sv.apiHumanPending)
	// reads that carry secrets: token-gated
	mux.HandleFunc("GET /api/human/answers", sv.authWrite(sv.apiHumanAnswers))
	// writes: token-gated — EXCEPT the human answer post (see below)
	mux.HandleFunc("POST /api/ingest", sv.authWrite(sv.apiIngest))
	mux.HandleFunc("POST /api/human", sv.authWrite(sv.apiAskHuman))
	mux.HandleFunc("POST /api/bank/status", sv.authWrite(sv.apiBankStatus))
	mux.HandleFunc("POST /api/human/{id}/answer", sv.apiHumanAnswer) // the human has no token
	return mux
}

// --- auth ----------------------------------------------------------------

func (sv *Server) authWrite(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if sv.tokenHash != "" && !store.TokenHashMatches(sv.tokenHash, r.Header.Get("X-Offerhound-Token")) {
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

func (sv *Server) apiUsers(w http.ResponseWriter, r *http.Request) {
	users, err := sv.Store.Users()
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	if users == nil {
		users = []string{}
	}
	jsonOut(w, users)
}

func (sv *Server) apiSearch(w http.ResponseWriter, r *http.Request) {
	qs := r.URL.Query()
	res, err := sv.Store.Search(store.Query{
		User:    qs.Get("user"),
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
	rows, err := sv.Store.Banks(r.URL.Query().Get("user"))
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	if rows == nil {
		rows = []store.BankRow{}
	}
	jsonOut(w, rows)
}

// apiIngest is THE write path: a client pushes everything it collected for
// one (user, bank) whenever it feels like it.
func (sv *Server) apiIngest(w http.ResponseWriter, r *http.Request) {
	var req engine.IngestReq
	if !decode(w, r, &req) {
		return
	}
	if req.Bank == "" || !bankIDRe.MatchString(req.Bank) {
		http.Error(w, "bad bank id", http.StatusBadRequest)
		return
	}
	if err := sv.Store.EnsureBank(req.User, req.Bank, req.Bank); err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	err := sv.Store.Ingest(req.User, req.Bank, req.Cards, req.Offers, req.Complete, req.Note)
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

// apiBankStatus lets a client report outcomes (needs_you / error) next to
// a bank without ingesting data.
func (sv *Server) apiBankStatus(w http.ResponseWriter, r *http.Request) {
	var body struct{ User, Bank, Status, Note string }
	if !decode(w, r, &body) {
		return
	}
	if !bankIDRe.MatchString(body.Bank) || body.Status == "" {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	if err := sv.Store.SetBankStatus(body.User, body.Bank, body.Status, body.Note); err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	jsonOut(w, map[string]any{"ok": true})
}

// apiAskHuman stores only the config-authored prompt string. Screenshots
// are deliberately not accepted: bank pages can show names, partial
// account numbers or balances, and no PII belongs in this database.
func (sv *Server) apiAskHuman(w http.ResponseWriter, r *http.Request) {
	var body struct {
		User   string `json:"user"`
		Bank   string `json:"bank"`
		Prompt string `json:"prompt"`
	}
	if !decode(w, r, &body) {
		return
	}
	if !bankIDRe.MatchString(body.Bank) || strings.TrimSpace(body.Prompt) == "" {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	id, err := sv.Store.AskHuman(body.User, body.Bank, body.Prompt)
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	jsonOut(w, map[string]any{"id": id})
}

// apiHumanPending feeds the UI's needs-you banner (open).
func (sv *Server) apiHumanPending(w http.ResponseWriter, r *http.Request) {
	rows, err := sv.Store.PendingHuman(r.URL.Query().Get("user"))
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	if rows == nil {
		rows = []store.HumanRequest{}
	}
	jsonOut(w, rows)
}

// apiHumanAnswers delivers OTP answers to the waiting client. Token-gated:
// answers are codes, not marketing data.
func (sv *Server) apiHumanAnswers(w http.ResponseWriter, r *http.Request) {
	var ids []int64
	for _, s := range r.URL.Query()["waiting"] {
		if id, err := strconv.ParseInt(s, 10, 64); err == nil {
			ids = append(ids, id)
		}
	}
	answers, err := sv.Store.AnswersFor(ids)
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	if answers == nil {
		answers = []store.Answer{}
	}
	jsonOut(w, answers)
}

// apiHumanAnswer is the one "write" the human on the phone makes — the
// human has no token, so this stays open (answer values are OTP codes the
// human already possesses; worst abuse is burning a pending prompt).
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
