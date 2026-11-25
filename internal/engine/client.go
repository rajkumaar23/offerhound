package engine

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/rajkumaar23/offerhound/internal/offer"
	"github.com/rajkumaar23/offerhound/internal/store"
)

// Client talks to an offerhound serve instance (split mode: the browser
// host pushes to wherever the UI lives).
type Client struct {
	Base  string
	Token string
	HTTP  *http.Client
}

// NewClient builds a client with sane timeouts.
func NewClient(base, token string) *Client {
	return &Client{Base: strings.TrimSuffix(base, "/"), Token: token,
		HTTP: &http.Client{Timeout: 60 * time.Second}}
}

func (c *Client) do(ctx context.Context, method, path string, body any, out any) error {
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.Base+path, rd)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	if c.Token != "" {
		req.Header.Set("X-Offerhound-Token", c.Token)
	}
	res, err := c.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(res.Body, 8<<20))
	if res.StatusCode >= 300 {
		return fmt.Errorf("%s %s: %d %s", method, path, res.StatusCode, strings.TrimSpace(string(raw)))
	}
	if out != nil {
		return json.Unmarshal(raw, out)
	}
	return nil
}

// IngestReq is the payload for POST /api/ingest.
type IngestReq struct {
	Bank     string        `json:"bank"`
	Worker   string        `json:"worker"`
	Complete bool          `json:"complete"`
	Note     string        `json:"note"`
	Cards    []offer.Card  `json:"cards"`
	Offers   []offer.Offer `json:"offers"`
}

// Ingest pushes a completed (or partial) collection to the server.
func (c *Client) Ingest(ctx context.Context, req IngestReq) error {
	return c.do(ctx, "POST", "/api/ingest", req, nil)
}

// StartRun registers a run on the server and returns its id.
func (c *Client) StartRun(ctx context.Context, bank, worker string) (int64, error) {
	var out struct {
		RunID int64 `json:"runId"`
	}
	err := c.do(ctx, "POST", "/api/runs", map[string]string{"bank": bank, "worker": worker}, &out)
	return out.RunID, err
}

// FinishRun closes a run on the server.
func (c *Client) FinishRun(ctx context.Context, id int64, status, errMsg string, count int) error {
	return c.do(ctx, "POST", fmt.Sprintf("/api/runs/%d/finish", id),
		map[string]any{"status": status, "error": errMsg, "offerCount": count}, nil)
}

// PollResult carries the next claimed job plus any human answers.
type PollResult struct {
	Job     *store.Job     `json:"job"`
	Answers []store.Answer `json:"answers"`
}

// PollJobs claims a job for this worker and delivers human answers.
func (c *Client) PollJobs(ctx context.Context, worker string, banks []string, waiting []int64) (*PollResult, error) {
	q := url.Values{"worker": {worker}}
	for _, b := range banks {
		q.Add("banks", b)
	}
	for _, id := range waiting {
		q.Add("waiting", fmt.Sprint(id))
	}
	var out PollResult
	err := c.do(ctx, "GET", "/api/jobs?"+q.Encode(), nil, &out)
	return &out, err
}

// CompleteJob reports a job's outcome.
func (c *Client) CompleteJob(ctx context.Context, id int64, status, errMsg string) error {
	return c.do(ctx, "POST", fmt.Sprintf("/api/jobs/%d/complete", id),
		map[string]string{"status": status, "error": errMsg}, nil)
}

// AskHumanRemote registers a 2FA prompt on the server (surfaced in the UI).
func (c *Client) AskHumanRemote(ctx context.Context, runID int64, bank, prompt string, screenshot []byte) (int64, error) {
	var out struct {
		ID int64 `json:"id"`
	}
	err := c.do(ctx, "POST", "/api/human", map[string]any{
		"runId": runID, "bank": bank, "prompt": prompt,
		"screenshotB64": base64.StdEncoding.EncodeToString(screenshot),
	}, &out)
	return out.ID, err
}

// ServerGate implements Gate by relaying to the serve instance and polling
// for the human's answer through the jobs poll (outbound-only).
type ServerGate struct {
	Client *Client
	RunID  int64
	Worker string
	Banks  []string
	Poll   time.Duration
}

func (g *ServerGate) Ask(ctx context.Context, bank, prompt string, screenshot []byte, timeout time.Duration) (string, error) {
	if g.Poll == 0 {
		g.Poll = 3 * time.Second
	}
	id, err := g.Client.AskHumanRemote(ctx, g.RunID, bank, prompt, screenshot)
	if err != nil {
		return "", err
	}
	deadline := time.Now().Add(timeout)
	for {
		pr, err := g.Client.PollJobs(ctx, g.Worker, g.Banks, []int64{id})
		if err != nil {
			return "", err
		}
		for _, a := range pr.Answers {
			if a.ID == id {
				return a.Answer, nil
			}
		}
		if time.Now().After(deadline) {
			return "", fmt.Errorf("timed out waiting for human answer")
		}
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-time.After(g.Poll):
		}
	}
}
