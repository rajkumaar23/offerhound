// Package store is the SQLite persistence layer: banks, offers, runs,
// job queue, and human (2FA) requests. It is the single writer interface —
// both the local engine and remote workers land here through Ingest.
package store

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/rajkumaar23/offerhound/internal/offer"
	_ "modernc.org/sqlite"
)

// ErrPartialRejected is returned when a non-complete collection tries to
// replace a bank's offers that currently has data. Last-good data wins.
var ErrPartialRejected = errors.New("partial collection rejected: keeping last-known data")

// Store wraps the database handle.
type Store struct{ db *sql.DB }

// Open opens (creating if needed) the SQLite database at path.
func Open(path string) (*Store, error) {
	db, err := sql.Open("sqlite", path+"?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)")
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1) // sqlite: serialize writers
	s := &Store{db: db}
	if err := s.migrate(); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

func (s *Store) Close() error { return s.db.Close() }

const schema = `
CREATE TABLE IF NOT EXISTS banks (
  key        TEXT PRIMARY KEY,
  name       TEXT NOT NULL,
  status     TEXT NOT NULL DEFAULT 'never', -- ok|stale|partial|needs_you|error|never
  last_sync  TEXT,
  note       TEXT NOT NULL DEFAULT '',
  cards_json TEXT NOT NULL DEFAULT '[]'
);
CREATE TABLE IF NOT EXISTS offers (
  id          TEXT NOT NULL,
  bank        TEXT NOT NULL REFERENCES banks(key) ON DELETE CASCADE,
  card_label  TEXT NOT NULL DEFAULT '',
  merchant    TEXT NOT NULL,
  title       TEXT NOT NULL DEFAULT '',
  description TEXT NOT NULL DEFAULT '',
  reward_text TEXT NOT NULL DEFAULT '',
  reward_value REAL,
  channel     TEXT NOT NULL DEFAULT '',
  status      TEXT NOT NULL DEFAULT 'eligible',
  notes       TEXT NOT NULL DEFAULT '',
  terms_url   TEXT NOT NULL DEFAULT '',
  expiry_date TEXT NOT NULL DEFAULT '',
  source      TEXT NOT NULL DEFAULT '',
  detail_url  TEXT NOT NULL DEFAULT '',
  scraped_at  TEXT NOT NULL,
  PRIMARY KEY (bank, id)
);
CREATE INDEX IF NOT EXISTS idx_offers_bank ON offers(bank);
CREATE INDEX IF NOT EXISTS idx_offers_merchant ON offers(merchant);
CREATE TABLE IF NOT EXISTS runs (
  id        INTEGER PRIMARY KEY AUTOINCREMENT,
  bank      TEXT NOT NULL,
  worker    TEXT NOT NULL DEFAULT '',
  started   TEXT NOT NULL,
  finished  TEXT,
  status    TEXT NOT NULL DEFAULT 'running', -- running|ok|partial|needs_you|error
  error     TEXT NOT NULL DEFAULT '',
  offer_count INTEGER NOT NULL DEFAULT 0
);
CREATE TABLE IF NOT EXISTS jobs (
  id        INTEGER PRIMARY KEY AUTOINCREMENT,
  bank      TEXT NOT NULL,
  state     TEXT NOT NULL DEFAULT 'queued', -- queued|claimed|done|error
  worker    TEXT NOT NULL DEFAULT '',
  created   TEXT NOT NULL,
  claimed   TEXT,
  finished  TEXT,
  error     TEXT NOT NULL DEFAULT ''
);
CREATE TABLE IF NOT EXISTS human_requests (
  id        INTEGER PRIMARY KEY AUTOINCREMENT,
  run_id    INTEGER NOT NULL REFERENCES runs(id) ON DELETE CASCADE,
  bank      TEXT NOT NULL,
  prompt    TEXT NOT NULL,
  screenshot BLOB,
  state     TEXT NOT NULL DEFAULT 'pending', -- pending|answered|expired
  answer    TEXT NOT NULL DEFAULT '',
  created   TEXT NOT NULL,
  responded TEXT
);
`

func (s *Store) migrate() error {
	_, err := s.db.Exec(schema)
	return err
}

func now() string { return time.Now().UTC().Format(time.RFC3339) }

// EnsureBank upserts the bank row metadata.
func (s *Store) EnsureBank(key, name string) error {
	_, err := s.db.Exec(`INSERT INTO banks(key, name) VALUES(?, ?)
		ON CONFLICT(key) DO UPDATE SET name=excluded.name`, key, name)
	return err
}

// Ingest merges a bank's offers atomically. A complete sync replaces the
// bank's offers; a partial sync upserts what it collected and marks the
// bank stale without touching untouched rows. An empty payload is always
// rejected — last-good data is sacred.
func (s *Store) Ingest(bank string, cards []offer.Card, offers []offer.Offer, complete bool, note string) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	// An unknown bank registers itself (name later refined by EnsureBank
	// from the config) so ingest can never die on a cold database.
	if _, err := tx.Exec(`INSERT INTO banks(key, name) VALUES(?, ?)
		ON CONFLICT(key) DO NOTHING`, bank, bank); err != nil {
		return err
	}

	if len(offers) == 0 {
		// Nothing new to merge — keep whatever is on disk untouched.
		return ErrPartialRejected
	}
	if complete {
		if _, err := tx.Exec(`DELETE FROM offers WHERE bank=?`, bank); err != nil {
			return err
		}
	}
	cardsJSON, _ := json.Marshal(cards)
	status := "ok"
	if !complete {
		status = "stale" // partial: some rows may predate this run
	}
	if len(cards) > 0 {
		if _, err := tx.Exec(`UPDATE banks SET status=?, last_sync=?, note=?, cards_json=? WHERE key=?`,
			status, now(), note, string(cardsJSON), bank); err != nil {
			return err
		}
	} else if _, err := tx.Exec(`UPDATE banks SET status=?, last_sync=?, note=? WHERE key=?`,
		status, now(), note, bank); err != nil {
		return err
	}
	stmt, err := tx.Prepare(`INSERT OR REPLACE INTO offers(id,bank,card_label,merchant,title,description,
		reward_text,reward_value,channel,status,notes,terms_url,expiry_date,source,detail_url,scraped_at)
		VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`)
	if err != nil {
		return err
	}
	defer stmt.Close()
	for _, o := range offers {
		if _, err := stmt.Exec(o.ID, bank, o.CardLabel, o.Merchant, o.Title, o.Description,
			o.RewardText, o.RewardValue, o.Channel, string(o.Status), o.Notes, o.TermsURL,
			o.ExpiryDate, o.Source, o.DetailURL, o.ScrapedAt.UTC().Format(time.RFC3339)); err != nil {
			return fmt.Errorf("insert offer %s: %w", o.ID, err)
		}
	}
	return tx.Commit()
}

// SetBankStatus records run outcomes outside of ingest (errors, needs_you).
func (s *Store) SetBankStatus(bank, status, note string) error {
	_, err := s.db.Exec(`UPDATE banks SET status=?, note=? WHERE key=?`, status, note, bank)
	return err
}

// BankRow is a bank plus its card metadata.
type BankRow struct {
	Key      string       `json:"key"`
	Name     string       `json:"name"`
	Status   string       `json:"status"`
	LastSync string       `json:"lastSync"`
	Note     string       `json:"note"`
	Cards    []offer.Card `json:"cards"`
}

// Banks lists all registered banks.
func (s *Store) Banks() ([]BankRow, error) {
	rows, err := s.db.Query(`SELECT key,name,status,COALESCE(last_sync,''),note,cards_json FROM banks ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []BankRow
	for rows.Next() {
		var b BankRow
		var cards string
		if err := rows.Scan(&b.Key, &b.Name, &b.Status, &b.LastSync, &b.Note, &cards); err != nil {
			return nil, err
		}
		_ = json.Unmarshal([]byte(cards), &b.Cards)
		out = append(out, b)
	}
	return out, rows.Err()
}

// Query filters the offer search.
type Query struct {
	Text    string
	Bank    string
	Status  string
	Channel string // "" any, "in-store" (matches In-Store/In-App/…& In-Store), "online"
	Limit   int
}

// Search returns offers matching the query, best matches first (exact
// merchant > prefix > substring; enrolled first inside a rank).
func (s *Store) Search(q Query) ([]offer.Offer, error) {
	var conds []string
	var args []any
	if strings.TrimSpace(q.Text) != "" {
		like := "%" + strings.ToLower(strings.TrimSpace(q.Text)) + "%"
		conds = append(conds, `(lower(merchant) LIKE ? OR lower(title) LIKE ? OR lower(description) LIKE ? OR lower(notes) LIKE ?)`)
		args = append(args, like, like, like, like)
	}
	if q.Bank != "" {
		conds = append(conds, `bank = ?`)
		args = append(args, q.Bank)
	}
	if q.Status != "" {
		conds = append(conds, `status = ?`)
		args = append(args, q.Status)
	}
	if q.Channel == "in-store" {
		conds = append(conds, `(channel LIKE '%store%' OR channel LIKE '%app%' OR channel LIKE '%In-App%')`)
	} else if q.Channel == "online" {
		conds = append(conds, `channel LIKE '%online%'`)
	}
	where := ""
	if len(conds) > 0 {
		where = "WHERE " + strings.Join(conds, " AND ")
	}
	limit := q.Limit
	if limit <= 0 || limit > 500 {
		limit = 200
	}
	rows, err := s.db.Query(`SELECT id,bank,card_label,merchant,title,description,reward_text,reward_value,
		channel,status,notes,terms_url,expiry_date,source,detail_url,scraped_at
		FROM offers `+where+` ORDER BY
		CASE WHEN status='enrolled' THEN 0 ELSE 1 END,
		CASE WHEN expiry_date != '' AND date(expiry_date) < date('now') THEN 1 ELSE 0 END,
		CASE WHEN expiry_date != '' THEN julianday(expiry_date) ELSE 9999999 END,
		merchant LIMIT ?`, append(args, limit)...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []offer.Offer
	for rows.Next() {
		var o offer.Offer
		var rv sql.NullFloat64
		var scrapedAt string
		if err := rows.Scan(&o.ID, &o.Bank, &o.CardLabel, &o.Merchant, &o.Title, &o.Description,
			&o.RewardText, &rv, &o.Channel, &o.Status, &o.Notes, &o.TermsURL,
			&o.ExpiryDate, &o.Source, &o.DetailURL, &scrapedAt); err != nil {
			return nil, err
		}
		o.RewardValue = rv.Float64
		o.ScrapedAt, _ = time.Parse(time.RFC3339, scrapedAt)
		out = append(out, o)
	}
	return out, rows.Err()
}

// --- runs ---------------------------------------------------------------

// StartRun records the beginning of a sync.
func (s *Store) StartRun(bank, worker string) (int64, error) {
	if _, err := s.db.Exec(`INSERT INTO banks(key, name) VALUES(?, ?)
		ON CONFLICT(key) DO NOTHING`, bank, bank); err != nil {
		return 0, err
	}
	res, err := s.db.Exec(`INSERT INTO runs(bank,worker,started,status) VALUES(?,?,?,'running')`,
		bank, worker, now())
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

// FinishRun closes a run.
func (s *Store) FinishRun(id int64, status, errMsg string, offerCount int) error {
	_, err := s.db.Exec(`UPDATE runs SET finished=?, status=?, error=?, offer_count=? WHERE id=?`,
		now(), status, errMsg, offerCount, id)
	return err
}

// --- human requests (2FA) -------------------------------------------------

// AskHuman registers a pending human request (used by engine via StoreGate
// or by remote workers via the API).
func (s *Store) AskHuman(runID int64, bank, prompt string, screenshot []byte) (int64, error) {
	res, err := s.db.Exec(`INSERT INTO human_requests(run_id,bank,prompt,screenshot,state,created)
		VALUES(?,?,?,?, 'pending', ?)`, runID, bank, prompt, screenshot, now())
	if err != nil {
		return 0, err
	}
	id, err := res.LastInsertId()
	return id, err
}

// PendingHuman lists pending human requests (UI + worker polling).
func (s *Store) PendingHuman() ([]HumanRequest, error) {
	rows, err := s.db.Query(`SELECT id,run_id,bank,prompt,state,answer,created FROM human_requests
		WHERE state='pending' ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []HumanRequest
	for rows.Next() {
		var h HumanRequest
		if err := rows.Scan(&h.ID, &h.RunID, &h.Bank, &h.Prompt, &h.State, &h.Answer, &h.Created); err != nil {
			return nil, err
		}
		out = append(out, h)
	}
	return out, rows.Err()
}

// HumanRequest is a 2FA prompt awaiting a human answer.
type HumanRequest struct {
	ID      int64  `json:"id"`
	RunID   int64  `json:"runId"`
	Bank    string `json:"bank"`
	Prompt  string `json:"prompt"`
	State   string `json:"state"`
	Answer  string `json:"answer"`
	Created string `json:"created"`
}

// AnswerHuman records a human's answer. Returns false if already resolved.
func (s *Store) AnswerHuman(id int64, answer string) (bool, error) {
	res, err := s.db.Exec(`UPDATE human_requests SET state='answered', answer=?, responded=?
		WHERE id=? AND state='pending'`, answer, now(), id)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n == 1, nil
}

// WaitForAnswer blocks until answered/expired/cancelled. Poll-based; human
// pace means 500 ms resolution is plenty.
func (s *Store) WaitForAnswer(ctx context.Context, id int64, timeout time.Duration) (string, error) {
	deadline := time.Now().Add(timeout)
	for {
		var state, answer string
		err := s.db.QueryRow(`SELECT state, answer FROM human_requests WHERE id=?`, id).Scan(&state, &answer)
		if errors.Is(err, sql.ErrNoRows) {
			return "", fmt.Errorf("human request %d vanished", id)
		}
		if err != nil {
			return "", err
		}
		if state == "answered" {
			return answer, nil
		}
		if time.Now().After(deadline) {
			s.db.Exec(`UPDATE human_requests SET state='expired' WHERE id=?`, id)
			return "", fmt.Errorf("timed out waiting for human answer (request %d)", id)
		}
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-time.After(500 * time.Millisecond):
		}
	}
}

// --- job queue (split mode) ------------------------------------------------

// EnqueueJob queues a sync for a worker to claim.
func (s *Store) EnqueueJob(bank string) (int64, error) {
	res, err := s.db.Exec(`INSERT INTO jobs(bank,state,created) VALUES(?,'queued',?)`, bank, now())
	if err != nil {
		return 0, err
	}
	id, err := res.LastInsertId()
	return id, err
}

// Job is a queued sync assignment.
type Job struct {
	ID   int64  `json:"id"`
	Bank string `json:"bank"`
}

// ClaimJob atomically hands the oldest queued job for one of the worker's
// banks. Returns nil when nothing is available.
func (s *Store) ClaimJob(worker string, banks []string) (*Job, error) {
	if len(banks) == 0 {
		return nil, nil
	}
	tx, err := s.db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	placeholders := strings.Repeat("?,", len(banks))
	placeholders = placeholders[:len(placeholders)-1]
	args := make([]any, len(banks))
	for i, b := range banks {
		args[i] = b
	}
	var j Job
	err = tx.QueryRow(`SELECT id, bank FROM jobs WHERE state='queued' AND bank IN (`+placeholders+`)
		ORDER BY id LIMIT 1`, args...).Scan(&j.ID, &j.Bank)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if _, err := tx.Exec(`UPDATE jobs SET state='claimed', worker=?, claimed=? WHERE id=? AND state='queued'`,
		worker, now(), j.ID); err != nil {
		return nil, err
	}
	return &j, tx.Commit()
}

// CompleteJob closes a claimed job.
func (s *Store) CompleteJob(id int64, status, errMsg string) error {
	_, err := s.db.Exec(`UPDATE jobs SET state=?, finished=?, error=? WHERE id=?`, status, now(), errMsg, id)
	return err
}

// Answer is a delivered human response (e.g. an OTP code).
type Answer struct {
	ID     int64  `json:"id"`
	Answer string `json:"answer"`
}

// AnswersFor returns answers for the given pending request ids.
func (s *Store) AnswersFor(ids []int64) ([]Answer, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	var conds []string
	args := make([]any, 0, len(ids))
	for _, id := range ids {
		conds = append(conds, "?")
		args = append(args, id)
	}
	rows, err := s.db.Query(`SELECT id, answer FROM human_requests
		WHERE state='answered' AND id IN (`+strings.Join(conds, ",")+`)`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Answer
	for rows.Next() {
		var a Answer
		if err := rows.Scan(&a.ID, &a.Answer); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// NewToken returns a hex token for API auth.
func NewToken() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}
