package engine

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"strings"
	"text/template"
	"time"

	"crypto/rand"

	"github.com/rajkumaar23/offerhound/internal/config"
	"github.com/rajkumaar23/offerhound/internal/offer"
	"github.com/tidwall/gjson"
)

// Result of a bank sync.
type Result struct {
	Cards      []offer.Card
	Offers     []offer.Offer
	Complete   bool
	SkippedLog []string // per-step non-fatal notes (rate limits, empty sources)
	Err        error
}

// Runner executes one bank's flows.
type Runner struct {
	Bank    *config.Bank
	Browser Browser
	Gate    Gate      // nil => human steps fail fast
	Log     io.Writer // progress (may be io.Discard)
	Now     func() time.Time
}

// maxRetryWait caps how much of a 429 Retry-After we ever sleep: portals
// advertise day-long values for soft blocks that clear in minutes (a human
// reload works immediately). var so tests can shrink it.
var maxRetryWait = 2 * time.Minute

func (r *Runner) logf(f string, a ...any) {
	if r.Log != nil {
		fmt.Fprintf(r.Log, "[%s] "+f+"\n", append([]any{r.Bank.ID}, a...)...)
	}
}

func (r *Runner) now() time.Time {
	if r.Now != nil {
		return r.Now()
	}
	return time.Now().UTC()
}

// Run executes login (probe-gated) then all offers steps.
func (r *Runner) Run(ctx context.Context) Result {
	res := Result{Complete: true}
	if err := r.Browser.Navigate(ctx, r.Bank.BaseURL); err != nil {
		res.Err = fmt.Errorf("open base url: %w", err)
		return res
	}
	if r.Bank.Login != nil {
		signedIn, err := r.probe(ctx)
		if err != nil {
			res.Err = err
			return res
		}
		if !signedIn {
			r.logf("signed out; running login flow")
			if err := r.runSteps(ctx, r.Bank.Login.Steps); err != nil {
				res.Err = fmt.Errorf("login flow: %w", err)
				return res
			}
		} else {
			r.logf("session alive; skipping login")
		}
	}
	lim := newLimiter(r.Bank.RateLimit)
	saved := map[string][]byte{}
	for i, s := range r.Bank.Offers {
		f := s.FetchAPI
		if f == nil {
			continue
		}
		if err := lim.acquire(); err != nil {
			res.Err = err
			return res
		}
		if f.Each != nil {
			items, err := eachItems(saved[f.Each.From], f.Each.Items)
			if err != nil {
				res.Err = fmt.Errorf("offers[%d] each: %w", i, err)
				return res
			}
			for _, item := range items {
				vars := map[string]any{f.Each.As: jsonRawToAny(item)}
				if f.Each.CardLabel != "" {
					// cardLabel templates render over the ITEM itself (same
					// context contract as cards.cardLabel in mapCards), so
					// "{{.product.description}}" resolves — not "{{.acct.x}}".
					itemCtx := map[string]any{}
					_ = json.Unmarshal([]byte(item.Raw), &itemCtx)
					if lbl, e := renderTemplate(f.Each.CardLabel, itemCtx); e == nil && lbl != "" {
						vars["cardLabel"] = lbl
					}
				}
				if err := r.fetchExtract(ctx, f, vars, lim, &res, saved); err != nil {
					res.SkippedLog = append(res.SkippedLog, err.Error())
					res.Complete = false
					r.logf("offers[%d] each item failed: %v", i, err)
				}
			}
			continue
		}
		if err := r.fetchExtract(ctx, f, map[string]any{}, lim, &res, saved); err != nil {
			res.SkippedLog = append(res.SkippedLog, err.Error())
			res.Complete = false
			r.logf("offers[%d] failed: %v", i, err)
		}
	}
	res.Offers = offer.Dedupe(res.Offers)
	res.Complete = res.Complete && len(res.Offers) > 0
	return res
}

// fetchExtract renders the URL template, fetches (paginating when asked),
// optionally decodes devalue, saves raw, maps offers/cards.
func (r *Runner) fetchExtract(ctx context.Context, f *config.FetchAPISpec, vars map[string]any,
	lim *limiter, res *Result, saved map[string][]byte) error {

	u, err := renderTemplate(f.URL, vars)
	if err != nil {
		return fmt.Errorf("url template: %w", err)
	}
	cursor := ""
	pages := 0
	gave429Retry := false
	for {
		pages++
		pageURL := u
		if f.Paginate != nil && cursor != "" {
			sep := "?"
			if strings.Contains(pageURL, "?") {
				sep = "&"
			}
			pageURL += sep + f.Paginate.CursorParam + "=" + url.QueryEscape(cursor)
		}
		body, status, retryAfter, err := r.fetch(ctx, pageURL, f.Headers)
		if err != nil {
			return err
		}
		if status == 429 {
			lim.penalize() // slow every subsequent fetch, not just this retry
			wait := 30 * time.Second
			if d, e := time.ParseDuration(retryAfter + "s"); e == nil {
				wait = d
			}
			// Trust the header only up to the cap — day-long Retry-After
			// values sit on minutes-long soft blocks — then give the step
			// exactly one retry. (The old per-page allowance let
			// non-paginated steps loop on 429 forever.)
			if wait > maxRetryWait {
				r.logf("429 on %s: retry-after %vs exceeds trust cap; using %v", f.Name, wait.Seconds(), maxRetryWait)
				wait = maxRetryWait
			}
			if gave429Retry {
				return fmt.Errorf("rate limited (429) on %s after one retry", f.Name)
			}
			gave429Retry = true
			r.logf("429 rate limited; sleeping %v then retrying once", wait)
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(wait):
			}
			continue
		}
		if status < 200 || status >= 300 {
			return fmt.Errorf("fetchApi %s: status %d", f.Name, status)
		}
		lim.onSuccess()
		decodedBody, derr := decodeBody(body, f.Decode, f.Devalue)
		if derr != nil {
			return fmt.Errorf("decode (%s): %w", f.Name, derr)
		}
		body = decodedBody
		if f.SaveAs != "" {
			saved[f.SaveAs] = append(saved[f.SaveAs], []byte(body)...)
			if !f.Devalue && f.Decode == nil && f.SaveAs != "" { // keep only last page for non-paginated
				saved[f.SaveAs] = []byte(body)
			}
		}
		doc := []byte(body)
		if f.Cards != nil && pages == 1 {
			cards, err := mapCards(f.Cards, doc)
			if err == nil && len(cards) > 0 {
				res.Cards = append(res.Cards, cards...)
			}
		}
		if f.Extract != nil {
			cardLabel := "default"
			if lbl, ok := vars["cardLabel"].(string); ok && lbl != "" {
				cardLabel = lbl
			}
			offers, err := MapOffers(r.Bank.ID, cardLabel, doc, f.Extract, r.now())
			if err != nil {
				return fmt.Errorf("extract (%s): %w", f.Name, err)
			}
			if f.Detail != nil {
				r.enrichDetails(ctx, f, vars, offers, lim)
			}
			res.Offers = append(res.Offers, offers...)
		}
		if f.Paginate == nil || pages >= f.Paginate.MaxPages {
			return nil
		}
		next := gjson.GetBytes(doc, cleanPath(f.Paginate.CursorPath))
		if !next.Exists() || next.String() == "" {
			return nil
		}
		if next.String() == cursor {
			return nil // loop-back guard
		}
		cursor = next.String()
		if err := lim.acquire(); err != nil {
			return err
		}
	}
}

func escapeQuotes(s string) string {
	return strings.ReplaceAll(s, "'", `\u0027`)
}

func (r *Runner) fetch(ctx context.Context, u string, headers map[string]string) (body string, status int, retryAfter string, err error) {
	// An empty record, never null: bank pages instrument window.fetch
	// (a RUM vendor wraps fetch on one major issuer portal) and the wrapper type-checks
	// headers, throwing TypeError on null. Pinned by TestFetchExpression.
	h := "{}"
	if len(headers) > 0 {
		b, _ := json.Marshal(headers)
		h = string(b)
	}
	// The URL rides into the evaluated JS as a JSON string literal
	// (json.Marshal is injection-proof embedding: quotes, backslashes,
	// backticks and ${} all escape correctly) — never hand-quoted.
	uJSON, err := json.Marshal(u)
	if err != nil {
		return "", 0, "", fmt.Errorf("marshal url: %w", err)
	}
	// Single quotes are forced to \u0027 even though JSON permits them raw:
	// the literal lands inside evaluated JS, and quote-shape conservatism
	// there is cheap. (Structural JSON quotes are never affected — only
	// bytes inside already-escaped strings.)
	expr := fmt.Sprintf(`(async () => {
		const r = await fetch(%s, {credentials: 'include', headers: %s});
		const t = await r.text();
		return JSON.stringify({status: r.status, retryAfter: r.headers.get('retry-after') || '', body: t});
	})()`, escapeQuotes(string(uJSON)), escapeQuotes(h))
	raw, err := r.Browser.EvaluateJSON(ctx, expr)
	if err != nil {
		return "", 0, "", fmt.Errorf("evaluate fetch (%s): %w", u, err)
	}
	var out struct {
		Status     int    `json:"status"`
		RetryAfter string `json:"retryAfter"`
		Body       string `json:"body"`
	}
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		return "", 0, "", fmt.Errorf("fetch envelope: %w", err)
	}
	return out.Body, out.Status, out.RetryAfter, nil
}

func (r *Runner) probe(ctx context.Context) (bool, error) {
	p := r.Bank.Login.Probe
	if p == nil || p.FetchAPI == "" {
		return false, nil // no probe => always run login steps
	}
	abs := p.FetchAPI
	if !strings.HasPrefix(abs, "http") {
		abs = strings.TrimSuffix(r.Bank.BaseURL, "/") + p.FetchAPI
	}
	body, status, _, err := r.fetch(ctx, abs, nil)
	if err != nil {
		return false, nil // probe errors => treat as signed out
	}
	ok := false
	for _, s := range p.OKStatus {
		if s == status {
			ok = true
		}
	}
	if ok && p.HasPath != "" {
		// Cheap auth discriminator: signed-out portals often answer 200
		// with a login page (redirects are followed), so status alone lies.
		decoded, derr := decodeBody(body, p.Decode, false)
		if derr != nil {
			return false, nil
		}
		if v := gjson.GetBytes([]byte(decoded), cleanPath(p.HasPath)); !v.Exists() || v.String() == "null" {
			return false, nil
		}
	}
	return ok, nil
}

func (r *Runner) runSteps(ctx context.Context, steps []config.Step) error {
	for i, s := range steps {
		switch {
		case s.Navigate != nil:
			if err := r.Browser.Navigate(ctx, *s.Navigate); err != nil {
				return fmt.Errorf("navigate %s: %w", *s.Navigate, err)
			}
		case s.WaitSelector != nil:
			if err := r.Browser.WaitFor(ctx, s.WaitSelector.Selector, s.WaitSelector.JSPath, true, s.WaitSelector.Timeout.Dur()); err != nil {
				return fmt.Errorf("waitSelector %v: %w", s.WaitSelector, err)
			}
		case s.WaitNotVis != nil:
			if err := r.Browser.WaitFor(ctx, s.WaitNotVis.Selector, s.WaitNotVis.JSPath, false, s.WaitNotVis.Timeout.Dur()); err != nil {
				return fmt.Errorf("waitNotVisible: %w", err)
			}
		case s.Click != nil:
			to := s.Click.Timeout.Dur()
			if to == 0 {
				to = 10 * time.Second
			}
			if err := r.Browser.Click(ctx, s.Click.Selector, s.Click.JSPath, s.Click.Trusted, to); err != nil {
				return fmt.Errorf("click %v: %w", s.Click, err)
			}
		case s.Fill != nil:
			val := s.Fill.Value
			if s.Fill.ValueFrom != "" {
				ref, ok := r.Bank.Credentials[strings.TrimPrefix(s.Fill.ValueFrom, "credential.")]
				if !ok {
					return fmt.Errorf("unknown credential in valueFrom: %s", s.Fill.ValueFrom)
				}
				v, err := resolveCredential(ref)
				if err != nil {
					return err
				}
				val = v
			}
			if err := r.Browser.Fill(ctx, s.Fill.Selector, s.Fill.JSPath, val); err != nil {
				return fmt.Errorf("fill %s: %w", s.Fill.Selector, err)
			}
			if s.Fill.Submit != "" {
				if err := r.Browser.Click(ctx, s.Fill.Submit, "", false, 10*time.Second); err != nil {
					return fmt.Errorf("fill submit: %w", err)
				}
			}
		case s.Sleep > 0:
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(s.Sleep.Dur()):
			}
		case s.Reload != nil:
			cur, err := r.Browser.CurrentURL(ctx)
			if err == nil {
				err = r.Browser.Navigate(ctx, cur)
			}
			if err != nil {
				return fmt.Errorf("reload: %w", err)
			}
		case s.Human != nil:
			if s.Human.When != nil {
				err := r.Browser.WaitFor(ctx, s.Human.When.Selector, s.Human.When.JSPath, true, s.Human.When.Timeout.Dur())
				if err != nil {
					continue // challenge did not appear; move on
				}
			}
			if r.Gate == nil {
				return fmt.Errorf("human step requires a gate (run headfully or wire --server)")
			}
			// Prompt text only — the browser page is never captured or
			// relayed (bank screens carry account PII).
			answer, err := r.Gate.Ask(ctx, r.Bank.ID, s.Human.Prompt, s.Human.Timeout.Dur())
			if err != nil {
				return fmt.Errorf("human step: %w", err)
			}
			if s.Human.Fill != nil {
				if err := r.Browser.Fill(ctx, s.Human.Fill.Selector, s.Human.Fill.JSPath, answer); err != nil {
					return fmt.Errorf("human fill: %w", err)
				}
				sel := s.Human.Fill.Submit
				if sel != "" {
					if err := r.Browser.Click(ctx, sel, "", false, 10*time.Second); err != nil {
						return fmt.Errorf("human submit: %w", err)
					}
				}
			}
		default:
			return fmt.Errorf("steps[%d]: unsupported step in login flow", i)
		}
	}
	return nil
}

// --- helpers -------------------------------------------------------------

func mapCards(c *config.CardsSpec, doc []byte) ([]offer.Card, error) {
	items := getArr(doc, c.Each)
	if !items.IsArray() {
		return nil, fmt.Errorf("cards.each %q not an array", c.Each)
	}
	var out []offer.Card
	items.ForEach(func(_, item gjson.Result) bool {
		vars := map[string]any{}
		_ = json.Unmarshal([]byte(item.Raw), &vars)
		label, err := renderTemplate(c.CardLabel, vars)
		if err != nil || label == "" {
			label = item.Get("productName").String()
		}
		out = append(out, offer.Card{Label: label})
		return true
	})
	return out, nil
}

func eachItems(savedDoc []byte, itemsPath string) ([]gjson.Result, error) {
	if len(savedDoc) == 0 {
		return nil, fmt.Errorf("source document not saved")
	}
	r := getArr(savedDoc, itemsPath)
	if !r.IsArray() {
		return nil, fmt.Errorf("items path %q not an array", itemsPath)
	}
	return r.Array(), nil
}

func jsonRawToAny(r gjson.Result) any {
	var v any
	_ = json.Unmarshal([]byte(r.Raw), &v)
	return v
}

var tmplFuncs = template.FuncMap{
	"urlquery": url.QueryEscape,
	"uuid": func() string {
		b := make([]byte, 16)
		rand.Read(b)
		return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
	},
	"join": func(sep string, vals ...any) string { return strings.Trim(fmt.Sprint(vals), "[]") },
}

func renderTemplate(tmpl string, vars map[string]any) (string, error) {
	t, err := template.New("url").Funcs(tmplFuncs).Parse(tmpl)
	if err != nil {
		return "", err
	}
	var sb strings.Builder
	if err := t.Execute(&sb, vars); err != nil {
		return "", err
	}
	return sb.String(), nil
}

// limiter enforces per-run request budget + minimum pacing.
type limiter struct {
	max      int
	used     int
	base     time.Duration
	delay    time.Duration // current pacing; doubles on 429, decays back on success
	last     time.Time
	okStreak int
}

func newLimiter(rl config.RateLimit) *limiter {
	l := &limiter{max: rl.RequestsPerRun, delay: rl.DelayBetween.Dur()}
	if l.max <= 0 {
		l.max = 40
	}
	if l.delay < 500*time.Millisecond {
		l.delay = 500 * time.Millisecond
	}
	l.base = l.delay
	return l
}

func (l *limiter) acquire() error {
	if l.used >= l.max {
		return fmt.Errorf("per-run request budget (%d) exhausted", l.max)
	}
	if d := time.Since(l.last); d < l.delay {
		time.Sleep(l.delay - d)
	}
	l.used++
	l.last = time.Now()
	return nil
}

// penalize slows ALL subsequent fetches after a 429: the portals throttle
// per session/IP, so hammering at constant pace just burns budget on
// failures (observed on capitaloneoffers under Black Friday load — even
// the site's own UI 429s).
func (l *limiter) penalize() {
	l.okStreak = 0
	const cap = 30 * time.Second
	if l.delay < cap {
		l.delay *= 2
		if l.delay > cap {
			l.delay = cap
		}
	}
}

// onSuccess lets pacing recover once the portal is happy again.
func (l *limiter) onSuccess() {
	l.okStreak++
	if l.okStreak >= 40 && l.delay > l.base {
		l.delay /= 2
		if l.delay < l.base {
			l.delay = l.base
		}
		l.okStreak = 0
	}
}
