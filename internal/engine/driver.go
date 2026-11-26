// Package engine executes bank DSL flows against a real browser over CDP.
package engine

import (
	"context"
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"time"

	"github.com/chromedp/cdproto/input"
	"github.com/chromedp/cdproto/runtime"
	"github.com/chromedp/chromedp"
	"github.com/rajkumaar23/offerhound/internal/config"
)

// Browser abstracts the driver so flows can be unit tested against fakes.
type Browser interface {
	Navigate(ctx context.Context, url string) error
	// EvaluateJSON evaluates an async JS expression, awaiting the promise
	// and returning the JSON-stringified result.
	EvaluateJSON(ctx context.Context, expr string) (string, error)
	WaitFor(ctx context.Context, sel, jsPath string, mustExist bool, timeout time.Duration) error
	Click(ctx context.Context, sel, jsPath string, trusted bool, timeout time.Duration) error
	Fill(ctx context.Context, sel, jsPath, value string) error
	CurrentURL(ctx context.Context) (string, error)
	Close()
}

// NewBrowser builds a Browser for the bank's config: either launches a
// dedicated Chrome (headless/headful, persistent profile) or attaches to an
// already-running one via its CDP URL.
func NewBrowser(b config.BrowserConfig, baseURL string) (Browser, error) {
	switch b.Mode {
	case "attach":
		return attach(b)
	case "", "launch":
		return launch(b)
	default:
		return nil, fmt.Errorf("unknown browser mode %q", b.Mode)
	}
}

func attach(b config.BrowserConfig) (Browser, error) {
	alloc, allocCancel := chromedp.NewRemoteAllocator(context.Background(), b.CDPURL)
	return &cdpBrowser{alloc: alloc, allocCancel: allocCancel}, nil
}

func launch(b config.BrowserConfig) (Browser, error) {
	headless := b.Headless == nil || *b.Headless
	profile := b.ProfileDir
	if profile == "" {
		home, _ := os.UserHomeDir()
		profile = filepath.Join(home, ".offerhound", "profiles", "default")
	}
	if err := os.MkdirAll(profile, 0o700); err != nil {
		return nil, fmt.Errorf("profile dir: %w", err)
	}
	opts := append(chromedp.DefaultExecAllocatorOptions[:0],
		chromedp.UserDataDir(profile),
		chromedp.Flag("disable-blink-features", "AutomationControlled"),
		chromedp.Flag("no-default-browser-check", true),
		chromedp.Flag("no-first-run", true),
	)
	// Container escape hatches: CHROME_BIN pins the binary,
	// OFFERHOUND_BROWSER_FLAGS appends extra Chromium flags
	// (comma-separated, e.g. "--no-sandbox,--disable-dev-shm-usage" —
	// the official Docker image ships with these two set).
	if bin := os.Getenv("CHROME_BIN"); bin != "" {
		if _, err := os.Stat(bin); err != nil {
			return nil, fmt.Errorf("CHROME_BIN=%s is not readable: %w", bin, err)
		}
		opts = append(opts, chromedp.ExecPath(bin))
	}
	for name, value := range extraFlagsFromEnv() {
		opts = append(opts, chromedp.Flag(name, value))
	}
	if !headless {
		if _, noDisplay := displayMissing(); noDisplay {
			return nil, fmt.Errorf("headful browser requested but no X display: " +
				"set DISPLAY, run under xvfb-run (common in Docker/CI), set headless: true, " +
				"or use mode: attach — run `offerhound doctor` for a full environment check")
		}
		opts = append(opts, chromedp.Flag("headless", false))
	} else {
		// new headless keeps the closest fingerprint to a real browser
		opts = append(opts, chromedp.Flag("headless", "new"))
	}
	if b.UserAgent != "" {
		opts = append(opts, chromedp.UserAgent(b.UserAgent))
	}
	alloc, allocCancel := chromedp.NewExecAllocator(context.Background(), opts...)
	return &cdpBrowser{alloc: alloc, allocCancel: allocCancel}, nil
}

type cdpBrowser struct {
	alloc       context.Context
	allocCancel context.CancelFunc
	cancel      context.CancelFunc
	tab         context.Context
	tabOnce     bool
}

func (b *cdpBrowser) ensureTab(ctx context.Context) error {
	if b.tabOnce {
		return nil
	}
	b.tab, b.cancel = chromedp.NewContext(b.alloc)
	b.tabOnce = true
	return nil
}

func (b *cdpBrowser) Navigate(ctx context.Context, url string) error {
	if err := b.ensureTab(ctx); err != nil {
		return err
	}
	return chromedp.Run(b.tab, chromedp.Navigate(url), chromedp.WaitReady("body"))
}

func (b *cdpBrowser) EvaluateJSON(ctx context.Context, expr string) (string, error) {
	if err := b.ensureTab(ctx); err != nil {
		return "", err
	}
	var out string
	err := chromedp.Run(b.tab, chromedp.Evaluate(expr, &out,
		func(p *runtime.EvaluateParams) *runtime.EvaluateParams {
			return p.WithAwaitPromise(true)
		}))
	if err != nil {
		return "", err
	}
	return out, nil
}

func (b *cdpBrowser) WaitFor(ctx context.Context, sel, jsPath string, mustExist bool, timeout time.Duration) error {
	if err := b.ensureTab(ctx); err != nil {
		return err
	}
	cctx, cancel := context.WithTimeout(b.tab, timeout)
	defer cancel()
	if jsPath != "" {
		if mustExist {
			return chromedp.Run(cctx, chromedp.WaitVisible(jsPath, chromedp.ByJSPath))
		}
		return chromedp.Run(cctx, chromedp.WaitNotPresent(jsPath, chromedp.ByJSPath))
	}
	if mustExist {
		return chromedp.Run(cctx, chromedp.WaitVisible(sel))
	}
	return chromedp.Run(cctx, chromedp.WaitNotPresent(sel))
}

func (b *cdpBrowser) Click(ctx context.Context, sel, jsPath string, trusted bool, timeout time.Duration) error {
	if err := b.ensureTab(ctx); err != nil {
		return err
	}
	cctx, cancel := context.WithTimeout(b.tab, timeout+5*time.Second)
	defer cancel()
	humanPause(250, 600) // a beat before any click — machine-timed clicks
	// read as automation to behavioral bot filters.
	if !trusted {
		// native .click() via JS — cheap and sufficient for most buttons
		expr := fmt.Sprintf(`(() => { const el = %s; if (!el) return 'missing';
			el.click(); return 'ok'; })()`, targetJS(sel, jsPath))
		_, err := b.EvaluateJSON(cctx, expr)
		return err
	}
	// Trusted path: chromedp.Click dispatches real Input-domain mouse
	// events (unlike JS el.click()), which popup blockers accept.
	target, by := sel, chromedp.ByQuery
	if jsPath != "" {
		target, by = jsPath, chromedp.ByJSPath
	}
	return chromedp.Run(cctx,
		chromedp.WaitVisible(target, by),
		chromedp.Click(target, by))
}

func (b *cdpBrowser) Fill(ctx context.Context, sel, jsPath, value string) error {
	if err := b.ensureTab(ctx); err != nil {
		return err
	}
	if jsPath == "" {
		// Real form field: type like a person. Instant .value= writes are a
		// classic bot tell (Amex challenges fills that land too fast), so
		// focus + clear via JS, then per-rune CDP insertText with jittered
		// pauses. Missing element stays a silent no-op (flows rely on it).
		return b.typeInto(ctx, sel, value)
	}
	// Native-setter write + input/change events: React/Angular-compatible.
	expr := fmt.Sprintf(`(() => {
		const el = %s;
		if (!el) return 'missing';
		const setter = Object.getOwnPropertyDescriptor(
			el.tagName === 'TEXTAREA' ? HTMLTextAreaElement.prototype : HTMLInputElement.prototype,
			'value').set;
		setter.call(el, %s);
		el.dispatchEvent(new Event('input', {bubbles: true}));
		el.dispatchEvent(new Event('change', {bubbles: true}));
		return 'ok';
	})()`, targetJS(sel, jsPath), quote(value))
	_, err := b.EvaluateJSON(ctx, expr)
	return err
}

// typeInto focuses an existing input and types value char-by-char with
// human-ish delays. Returns nil (no-op) when the element is absent.
func (b *cdpBrowser) typeInto(ctx context.Context, sel, value string) error {
	focus := fmt.Sprintf(`(() => {
		const el = document.querySelector(%s);
		if (!el) return 'missing';
		el.scrollIntoView({block: 'center'});
		el.focus();
		const setter = Object.getOwnPropertyDescriptor(
			el.tagName === 'TEXTAREA' ? HTMLTextAreaElement.prototype : HTMLInputElement.prototype,
			'value').set;
		setter.call(el, '');
		el.dispatchEvent(new Event('input', {bubbles: true}));
		return 'ok';
	})()`, quote(sel))
	res, err := b.EvaluateJSON(ctx, focus)
	if err != nil || res == `"missing"` {
		return err
	}
	humanPause(350, 800) // "read the page" before touching the field
	for _, r := range value {
		if err := chromedp.Run(b.tab, input.InsertText(string(r))); err != nil {
			return err
		}
		humanPause(45, 140)
	}
	_, _ = b.EvaluateJSON(ctx, fmt.Sprintf(`(() => {
		const el = document.querySelector(%s);
		if (!el) return 'ok';
		el.dispatchEvent(new Event('input', {bubbles: true}));
		el.dispatchEvent(new Event('change', {bubbles: true}));
		return 'ok';
	})()`, quote(sel)))
	return nil
}

// humanPause sleeps a random span to break up machine-timed interactions.
func humanPause(minMs, maxMs int64) {
	d := minMs
	if maxMs > minMs {
		d += rand.Int63n(maxMs - minMs)
	}
	time.Sleep(time.Duration(d) * time.Millisecond)
}

func (b *cdpBrowser) CurrentURL(ctx context.Context) (string, error) {
	if err := b.ensureTab(ctx); err != nil {
		return "", err
	}
	var u string
	err := chromedp.Run(b.tab, chromedp.Location(&u))
	return u, err
}

func (b *cdpBrowser) Close() {
	if b.cancel != nil {
		b.cancel()
	}
	if b.allocCancel != nil {
		b.allocCancel()
	}
}

func targetJS(sel, jsPath string) string {
	if jsPath != "" {
		return "(" + jsPath + ")"
	}
	return "document.querySelector(" + quote(sel) + ")"
}

func quote(s string) string {
	return "`" + esc(s) + "`"
}

func esc(s string) string {
	out := make([]rune, 0, len(s))
	for _, r := range s {
		switch r {
		case '`', '\\', '$':
			out = append(out, '\\', r)
		case '\n':
			out = append(out, '\\', 'n')
		default:
			out = append(out, r)
		}
	}
	return string(out)
}
