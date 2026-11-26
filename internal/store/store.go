// Package store is the SQLite persistence layer: per-user wallets of
// banks, offers, and human (2FA) prompts. It is a dumb store — clients
// push whenever they want (cron, manual, CI); there is no server-side
// scheduler, job queue, or run bookkeeping.
//
// Multi-user model: every record carries a `user` discriminator ("default"
// for single-user installs). It is a convenience scoping key, NOT a
// security boundary — reads are open by design (aggregated offer lists are
// not private info), writes are gated by a token hash at the HTTP layer.
// No bank page screenshots or other potentially PII-bearing artifacts are
// ever captured or stored.
package store

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/rajkumaar23/offerhound/internal/offer"
	_ "modernc.org/sqlite"
)

// ErrPartialRejected is returned when an ingest carries no offers — a
// failed collection must never clobber last-known data.
var ErrPartialRejected = errors.New("partial collection rejected: keeping last-known data")

// DefaultUser is the wallet used when no user is specified.
const DefaultUser = "default"

// Store wraps the database handle.
type Store struct{ db *sql.DB }

// Open opens (creating if needed) the SQLite database at path.
func Open(path string) (*Store, error) {
	if dir := filepath.Dir(path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return nil, fmt.Errorf("db dir: %w", err)
		}
	}
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
  user       TEXT NOT NULL DEFAULT 'default',
  key        TEXT NOT NULL,
  name       TEXT NOT NULL,
  status     TEXT NOT NULL DEFAULT 'never', -- ok|stale|needs_you|error
  last_sync  TEXT,
  note       TEXT NOT NULL DEFAULT '',
  cards_json TEXT NOT NULL DEFAULT '[]',
  PRIMARY KEY (user, key)
);
CREATE TABLE IF NOT EXISTS offers (
  user        TEXT NOT NULL DEFAULT 'default',
  id          TEXT NOT NULL,
  bank        TEXT NOT NULL,
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
  PRIMARY KEY (user, bank, id),
  FOREIGN KEY (user, bank) REFERENCES banks(user, key) ON DELETE CASCADE
);
CREATE INDEX IF NOT EXISTS idx_offers_lookup ON offers(user, bank, merchant);
CREATE TABLE IF NOT EXISTS human_requests (
  id        INTEGER PRIMARY KEY AUTOINCREMENT,
  user      TEXT NOT NULL DEFAULT 'default',
  bank      TEXT NOT NULL,
  prompt    TEXT NOT NULL,
  state     TEXT NOT NULL DEFAULT 'pending', -- pending|answered|expired
  answer    TEXT NOT NULL DEFAULT '',
  created   TEXT NOT NULL,
  responded TEXT
);
`

// migrate creates the schema, upgrading older databases in place. Earlier
// revisions carried per-user-less tables plus runs/jobs queues and a
// screenshot column; all of that legacy state is dropped — the server
// keeps no scheduler or page artifacts.
func (s *Store) migrate() error {
	var cols int
	if err := s.db.QueryRow(`SELECT count(*) FROM pragma_table_info('banks') WHERE name='user'`).Scan(&cols); err != nil {
		return err
	}
	var tables int
	if err := s.db.QueryRow(`SELECT count(*) FROM sqlite_master WHERE type='table' AND name='offers'`).Scan(&tables); err != nil {
		return err
	}
	if tables > 0 && cols == 0 {
		// v1 (no user column) -> v3: rebuild the core tables.
		return s.rebuildV1()
	}
	if _, err := s.db.Exec(schema); err != nil {
		return err
	}
	for _, t := range []string{"jobs", "runs"} {
		if _, err := s.db.Exec(`DROP TABLE IF EXISTS ` + t); err != nil {
			return err
		}
	}
	var hrRunID int
	if err := s.db.QueryRow(`SELECT count(*) FROM pragma_table_info('human_requests') WHERE name='run_id'`).Scan(&hrRunID); err != nil {
		return err
	}
	if hrRunID == 0 {
		return nil // already v3
	}
	// v2 -> v3: rebuild human_requests without run_id/screenshot.
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`ALTER TABLE human_requests RENAME TO human_requests_v2`); err != nil {
		return err
	}
	if _, err := tx.Exec(schema); err != nil {
		return err
	}
	if _, err := tx.Exec(`INSERT INTO human_requests(id,user,bank,prompt,state,answer,created,responded)
		SELECT id,user,bank,prompt,state,answer,created,responded FROM human_requests_v2`); err != nil {
		return err
	}
	if _, err := tx.Exec(`DROP TABLE human_requests_v2`); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) rebuildV1() error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, t := range []string{"offers", "banks", "human_requests"} {
		if _, err := tx.Exec(`ALTER TABLE ` + t + ` RENAME TO ` + t + `_v1`); err != nil {
			return fmt.Errorf("rename %s: %w", t, err)
		}
	}
	if _, err := tx.Exec(schema); err != nil {
		return err
	}
	copies := []string{
		`INSERT INTO banks(user,key,name,status,last_sync,note,cards_json)
		 SELECT 'default',key,name,status,last_sync,note,cards_json FROM banks_v1`,
		`INSERT INTO offers(user,id,bank,card_label,merchant,title,description,reward_text,
			reward_value,channel,status,notes,terms_url,expiry_date,source,detail_url,scraped_at)
		 SELECT 'default',id,bank,card_label,merchant,title,description,reward_text,
			reward_value,channel,status,notes,terms_url,expiry_date,source,detail_url,scraped_at FROM offers_v1`,
		`INSERT INTO human_requests(id,user,bank,prompt,state,answer,created,responded)
		 SELECT id,'default',bank,prompt,state,answer,created,responded FROM human_requests_v1`,
	}
	for _, q := range copies {
		if _, err := tx.Exec(q); err != nil {
			return fmt.Errorf("copy: %w", err)
		}
	}
	for _, t := range []string{"offers_v1", "banks_v1", "human_requests_v1", "jobs", "runs"} {
		if _, err := tx.Exec(`DROP TABLE IF EXISTS ` + t); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func now() string { return time.Now().UTC().Format(time.RFC3339) }

// userSafe keeps the discriminator sane.
func userSafe(u string) string {
	u = strings.TrimSpace(u)
	if u == "" {
		return DefaultUser
	}
	if len(u) > 64 {
		return u[:64]
	}
	return u
}

// EnsureBank upserts the bank row metadata for a user's wallet.
func (s *Store) EnsureBank(user, key, name string) error {
	_, err := s.db.Exec(`INSERT INTO banks(user,key,name) VALUES(?,?,?)
		ON CONFLICT(user,key) DO UPDATE SET name=excluded.name`, userSafe(user), key, name)
	return err
}

// Users lists every wallet that has banks registered (UI dropdown).
func (s *Store) Users() ([]string, error) {
	rows, err := s.db.Query(`SELECT DISTINCT user FROM banks ORDER BY user`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var u string
		if err := rows.Scan(&u); err != nil {
			return nil, err
		}
		out = append(out, u)
	}
	return out, rows.Err()
}

// Ingest merges a bank's offers into one user's wallet atomically. A
// complete sync replaces the bank's offers; a partial sync upserts what it
// collected and marks the bank stale without touching untouched rows. An
// empty payload is always rejected — last-good data is sacred.
func (s *Store) Ingest(user, bank string, cards []offer.Card, offers []offer.Offer, complete bool, note string) error {
	user = userSafe(user)
	if len(offers) == 0 {
		return ErrPartialRejected
	}
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	// An unknown bank registers itself (name later refined by EnsureBank
	// from the config) so ingest can never die on a cold database.
	if _, err := tx.Exec(`INSERT INTO banks(user,key,name) VALUES(?,?,?)
		ON CONFLICT(user,key) DO NOTHING`, user, bank, bank); err != nil {
		return err
	}
	if complete {
		if _, err := tx.Exec(`DELETE FROM offers WHERE user=? AND bank=?`, user, bank); err != nil {
			return err
		}
	}
	cardsJSON, _ := json.Marshal(cards)
	status := "ok"
	if !complete {
		status = "stale" // partial: some rows may predate this run
	}
	if len(cards) > 0 {
		if _, err := tx.Exec(`UPDATE banks SET status=?, last_sync=?, note=?, cards_json=?
			WHERE user=? AND key=?`, status, now(), note, string(cardsJSON), user, bank); err != nil {
			return err
		}
	} else if _, err := tx.Exec(`UPDATE banks SET status=?, last_sync=?, note=?
		WHERE user=? AND key=?`, status, now(), note, user, bank); err != nil {
		return err
	}
	stmt, err := tx.Prepare(`INSERT OR REPLACE INTO offers(user,id,bank,card_label,merchant,title,description,
		reward_text,reward_value,channel,status,notes,terms_url,expiry_date,source,detail_url,scraped_at)
		VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`)
	if err != nil {
		return err
	}
	defer stmt.Close()
	for _, o := range offers {
		if _, err := stmt.Exec(user, o.ID, bank, o.CardLabel, o.Merchant, o.Title, o.Description,
			o.RewardText, o.RewardValue, o.Channel, string(o.Status), o.Notes, o.TermsURL,
			o.ExpiryDate, o.Source, o.DetailURL, o.ScrapedAt.UTC().Format(time.RFC3339)); err != nil {
			return fmt.Errorf("insert offer %s: %w", o.ID, err)
		}
	}
	return tx.Commit()
}

// SetBankStatus records client-reported outcomes (errors, needs_you).
func (s *Store) SetBankStatus(user, bank, status, note string) error {
	_, err := s.db.Exec(`UPDATE banks SET status=?, note=? WHERE user=? AND key=?`,
		status, note, userSafe(user), bank)
	return err
}

// BankRow is a bank plus its card metadata.
type BankRow struct {
	User     string       `json:"user"`
	Key      string       `json:"key"`
	Name     string       `json:"name"`
	Status   string       `json:"status"`
	LastSync string       `json:"lastSync"`
	Note     string       `json:"note"`
	Cards    []offer.Card `json:"cards"`
}

// Banks lists one wallet's banks.
func (s *Store) Banks(user string) ([]BankRow, error) {
	rows, err := s.db.Query(`SELECT user,key,name,status,COALESCE(last_sync,''),note,cards_json
		FROM banks WHERE user=? ORDER BY name`, userSafe(user))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []BankRow
	for rows.Next() {
		var b BankRow
		var cardsJSON string
		if err := rows.Scan(&b.User, &b.Key, &b.Name, &b.Status, &b.LastSync, &b.Note, &cardsJSON); err != nil {
			return nil, err
		}
		_ = json.Unmarshal([]byte(cardsJSON), &b.Cards)
		out = append(out, b)
	}
	return out, rows.Err()
}

// Query filters the offer search.
type Query struct {
	User    string
	Text    string
	Bank    string
	Status  string
	Channel string // "" any, "in-store", "online"
	Limit   int
}

// Search returns one wallet's offers matching the query. Sort order:
// enrolled first, then soonest expiry, expired last, merchant A-Z.
func (s *Store) Search(q Query) ([]offer.Offer, error) {
	conds := []string{"user = ?"}
	args := []any{userSafe(q.User)}
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
	where := "WHERE " + strings.Join(conds, " AND ")
	limit := q.Limit
	if limit <= 0 || limit > 5000 {
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

// HumanRequest is a 2FA prompt awaiting a human answer. Only the
// config-authored prompt text is stored — never screenshots or page data.
type HumanRequest struct {
	ID      int64  `json:"id"`
	User    string `json:"user"`
	Bank    string `json:"bank"`
	Prompt  string `json:"prompt"`
	State   string `json:"state"`
	Answer  string `json:"answer"`
	Created string `json:"created"`
}

// AskHuman opens a human request for one wallet.
func (s *Store) AskHuman(user, bank, prompt string) (int64, error) {
	res, err := s.db.Exec(`INSERT INTO human_requests(user,bank,prompt,state,created)
		VALUES(?,?,?,'pending',?)`, userSafe(user), bank, prompt, now())
	if err != nil {
		return 0, err
	}
	id, err := res.LastInsertId()
	return id, err
}

// PendingHuman lists one wallet's pending human requests (UI polling).
func (s *Store) PendingHuman(user string) ([]HumanRequest, error) {
	rows, err := s.db.Query(`SELECT id,user,bank,prompt,state,answer,created
		FROM human_requests WHERE state='pending' AND user=? ORDER BY id`, userSafe(user))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []HumanRequest
	for rows.Next() {
		var h HumanRequest
		if err := rows.Scan(&h.ID, &h.User, &h.Bank, &h.Prompt, &h.State, &h.Answer, &h.Created); err != nil {
			return nil, err
		}
		out = append(out, h)
	}
	return out, rows.Err()
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

// AnswersFor returns answers for the given request ids (client polling for
// the OTP it is waiting on). Token-gated at the HTTP layer.
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

// Answer is a delivered human response (e.g. an OTP code).
type Answer struct {
	ID     int64  `json:"id"`
	Answer string `json:"answer"`
}

// --- token hashing (write gate) ----------------------------------------------

// NewToken returns a hex token for API writes.
func NewToken() (string, error) {
	b := make([]byte, 24)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// TokenHash is the hex SHA-256 of a token; only the hash needs to ever be
// stored or compared server-side.
func TokenHash(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

// TokenHashMatches constant-time compares a provided token against a
// stored SHA-256 hex hash.
func TokenHashMatches(storedHash, provided string) bool {
	want, err := hex.DecodeString(storedHash)
	if err != nil || len(want) != sha256.Size {
		return false
	}
	got := sha256.Sum256([]byte(provided))
	return subtle.ConstantTimeCompare(want, got[:]) == 1
}
