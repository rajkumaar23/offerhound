package engine

import (
	"context"
	"net/http"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"time"

	"github.com/chromedp/chromedp"
	"github.com/rajkumaar23/offerhound/internal/config"
)

// Environment preflight — the validations that turn "chromium failed
// mysteriously inside a container" into one actionable sentence.
//
// Offerhound's official Docker image bundles headless Chromium; headful
// automation inside a container is possible but needs an X server
// (xvfb-run). The doctor surfaces exactly which of these you have.

// Check is one preflight result.
type Check struct {
	Name   string `json:"name"`
	Status string `json:"status"` // ok|warn|fail
	Detail string `json:"detail"`
	Hint   string `json:"hint,omitempty"`
}

func ok(name, detail string) Check { return Check{Name: name, Status: "ok", Detail: detail} }
func warn(name, detail, hint string) Check {
	return Check{Name: name, Status: "warn", Detail: detail, Hint: hint}
}
func fail(name, detail, hint string) Check {
	return Check{Name: name, Status: "fail", Detail: detail, Hint: hint}
}

// InDocker reports whether we appear to run inside a container.
func InDocker() bool { _, err := os.Stat("/.dockerenv"); return err == nil }

func extraFlagsFromEnv() map[string]any {
	out := map[string]any{}
	for _, f := range strings.Split(os.Getenv("OFFERHOUND_BROWSER_FLAGS"), ",") {
		f = strings.TrimSpace(f)
		if f == "" {
			continue
		}
		name, value := strings.TrimPrefix(f, "--"), any(true)
		if i := strings.Index(name, "="); i > 0 {
			name, value = name[:i], name[i+1:]
		}
		out[name] = value
	}
	return out
}

// displayMissing reports (hasXAuth, noDisplay). Headful Chrome on Linux
// needs a display; xvfb-run sets DISPLAY for you.
func displayMissing() (xauth bool, missing bool) {
	if runtime.GOOS != "linux" {
		return true, false
	}
	disp := os.Getenv("DISPLAY")
	way := os.Getenv("WAYLAND_DISPLAY")
	return disp != "" || way != "", disp == "" && way == ""
}

// FindBrowser resolves the Chromium binary the same way launch() does.
func FindBrowser() (string, bool) {
	if bin := os.Getenv("CHROME_BIN"); bin != "" {
		if _, err := os.Stat(bin); err == nil {
			return bin, true
		}
		return bin, false
	}
	for _, cand := range []string{
		"google-chrome", "google-chrome-stable", "chrome",
		"chromium", "chromium-browser",
	} {
		if p, err := exec.LookPath(cand); err == nil {
			return p, true
		}
	}
	return "", false
}

// PreflightOpts configures the doctor run.
type PreflightOpts struct {
	Banks     []config.Bank // loaded configs (may be empty)
	Live      bool          // actually launch Chromium (slower, thorough)
	HTTPProbe string        // optional URL that must answer 200 (serve healthcheck)
}

// Preflight runs every environment check. Failures are advisory except
// where they make the requested operation impossible.
func Preflight(o PreflightOpts) []Check {
	var checks []Check
	in := InDocker()
	if in {
		checks = append(checks, ok("container", "running inside Docker"))
	}

	bin, found := FindBrowser()
	switch {
	case !found && o.GetenvLike():
		checks = append(checks, fail("chromium", "CHROME_BIN points at a missing file",
			"install chromium or fix CHROME_BIN"))
	case !found:
		hint := "install Google Chrome or Chromium on this host"
		if in {
			hint = "use the official ghcr.io image (bundles chromium) or install it in your derived image"
		}
		checks = append(checks, warn("chromium", "no Chrome/Chromium binary found", hint))
	default:
		checks = append(checks, ok("chromium", bin))
	}

	// sandbox: root + sandbox = the classic container crash loop
	if os.Geteuid() == 0 {
		if _, noSandbox := extraFlagsFromEnv()["no-sandbox"]; !noSandbox {
			checks = append(checks, warn("sandbox", "running as root without --no-sandbox",
				"chromium will fail to start: set OFFERHOUND_BROWSER_FLAGS=--no-sandbox,--disable-dev-shm-usage"))
		} else {
			checks = append(checks, ok("sandbox", "root + --no-sandbox (container default)"))
		}
	}

	// headful feasibility per bank that asks for it
	if _, missing := displayMissing(); missing {
		for _, b := range o.Banks {
			if b.Browser.Mode == "launch" && b.Browser.Headless != nil && !*b.Browser.Headless {
				checks = append(checks, fail("display:"+b.ID,
					"bank wants a visible browser but no X/Wayland display",
					"run headless, use xvfb-run (containers/CI), or mode: attach to a real browser"))
			}
		}
	}

	if o.Live && found {
		if err := launchProbe(); err != nil {
			checks = append(checks, fail("launch", "headless launch probe failed: "+err.Error(),
				"common causes: missing shared libs (install chromium deps), shm too small "+
					"(docker run --shm-size=256m), sandbox (see sandbox check)"))
		} else {
			checks = append(checks, ok("launch", "headless chromium launched, evaluated JS, exited clean"))
		}
	}

	if o.HTTPProbe != "" {
		client := &http.Client{Timeout: 5 * time.Second}
		resp, err := client.Get(o.HTTPProbe)
		if err != nil {
			checks = append(checks, fail("http", o.HTTPProbe+" unreachable: "+err.Error(), ""))
		} else {
			resp.Body.Close()
			if resp.StatusCode != http.StatusOK {
				checks = append(checks, fail("http", o.HTTPProbe+" answered "+resp.Status, ""))
			} else {
				checks = append(checks, ok("http", o.HTTPProbe+" 200"))
			}
		}
	}

	return checks
}

// GetenvLike: CHROME_BIN set but broken counts as a hard fail (the user
// explicitly pinned a binary that doesn't exist).
func (o PreflightOpts) GetenvLike() bool {
	bin := os.Getenv("CHROME_BIN")
	return bin != "" && fileMissing(bin)
}

func fileMissing(p string) bool { _, err := os.Stat(p); return err != nil }

// launchProbe boots headless chromium once, runs 1+1, tears down.
func launchProbe() error {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	opts := append(chromedp.DefaultExecAllocatorOptions[:0],
		chromedp.Flag("headless", "new"),
		chromedp.Flag("no-first-run", true),
	)
	if bin, okFound := FindBrowser(); okFound {
		if os.Getenv("CHROME_BIN") != "" {
			opts = append(opts, chromedp.ExecPath(bin))
		}
	}
	for name, value := range extraFlagsFromEnv() {
		opts = append(opts, chromedp.Flag(name, value))
	}
	alloc, allocCancel := chromedp.NewExecAllocator(ctx, opts...)
	defer allocCancel()
	tctx, tabCancel := chromedp.NewContext(alloc)
	defer tabCancel()
	var out int
	return chromedp.Run(tctx,
		chromedp.Navigate("about:blank"),
		chromedp.Evaluate(`1+1`, &out),
	)
}

// PreflightFailed reports whether any check is a hard failure.
func PreflightFailed(checks []Check) bool {
	for _, c := range checks {
		if c.Status == "fail" {
			return true
		}
	}
	return false
}
