package engine

import (
	"bytes"
	"context"
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

// Client talks to an offerhound serve instance. The server is dumb: this
// client pushes whenever it wants (cron, manual sync). Writes carry the
// plaintext token; the server stores only its hash.
type Client struct {
	Base  string
	Token string
	HTTP  *http.Client
}

// NewClient builds a client with sane timeouts. The write token never
// travels beyond the home host: on any cross-host redirect it is stripped
// (Go forwards custom headers across redirects otherwise).
func NewClient(base, token string) *Client {
	hc := &http.Client{Timeout: 60 * time.Second}
	hc.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		if len(via) > 0 && req.URL.Host != via[0].URL.Host {
			req.Header.Del("X-Offerhound-Token")
		}
		if len(via) >= 5 {
			return http.ErrUseLastResponse
		}
		return nil
	}
	return &Client{Base: strings.TrimSuffix(base, "/"), Token: token, HTTP: hc}
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
	User     string        `json:"user"`
	Bank     string        `json:"bank"`
	Worker   string        `json:"worker"`
	Complete bool          `json:"complete"`
	Note     string        `json:"note"`
	Cards    []offer.Card  `json:"cards"`
	Offers   []offer.Offer `json:"offers"`
}

// Ingest pushes a completed (or partial) collection into one wallet.
func (c *Client) Ingest(ctx context.Context, req IngestReq) error {
	return c.do(ctx, "POST", "/api/ingest", req, nil)
}

// SetBankStatus reports a client-side outcome (needs_you, error).
func (c *Client) SetBankStatus(ctx context.Context, user, bank, status, note string) error {
	return c.do(ctx, "POST", "/api/bank/status", map[string]string{
		"user": user, "bank": bank, "status": status, "note": note,
	}, nil)
}

// AskHumanRemote registers a 2FA prompt on the server (surfaced in the UI).
// Only the config-authored prompt text is sent — never screenshots or page
// content, which could carry account PII.
func (c *Client) AskHumanRemote(ctx context.Context, user, bank, prompt string) (int64, error) {
	var out struct {
		ID int64 `json:"id"`
	}
	err := c.do(ctx, "POST", "/api/human", map[string]any{
		"user": user, "bank": bank, "prompt": prompt,
	}, &out)
	return out.ID, err
}

// Answers fetches delivered answers for the given request ids.
func (c *Client) Answers(ctx context.Context, ids []int64) ([]store.Answer, error) {
	q := url.Values{}
	for _, id := range ids {
		q.Add("waiting", fmt.Sprint(id))
	}
	var out []store.Answer
	err := c.do(ctx, "GET", "/api/human/answers?"+q.Encode(), nil, &out)
	return out, err
}

// ServerGate implements Gate by relaying prompts to the serve instance and
// polling for the human's answer (outbound-only).
type ServerGate struct {
	Client *Client
	User   string
	Poll   time.Duration
}

func (g *ServerGate) Ask(ctx context.Context, bank, prompt string, timeout time.Duration) (string, error) {
	if g.Poll == 0 {
		g.Poll = 3 * time.Second
	}
	id, err := g.Client.AskHumanRemote(ctx, g.User, bank, prompt)
	if err != nil {
		return "", err
	}
	deadline := time.Now().Add(timeout)
	for {
		answers, err := g.Client.Answers(ctx, []int64{id})
		if err != nil {
			return "", err
		}
		for _, a := range answers {
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
