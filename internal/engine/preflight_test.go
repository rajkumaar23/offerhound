package engine

import (
	"runtime"
	"testing"

	"github.com/rajkumaar23/offerhound/internal/config"
)

func TestExtraFlagsFromEnv(t *testing.T) {
	t.Setenv("OFFERHOUND_BROWSER_FLAGS", "--no-sandbox, --disable-dev-shm-usage,,--log-level=0")
	got := extraFlagsFromEnv()
	if got["no-sandbox"] != any(true) {
		t.Fatalf("bool flag: %v", got)
	}
	if got["disable-dev-shm-usage"] != any(true) {
		t.Fatalf("second flag: %v", got)
	}
	if got["log-level"] != any("0") {
		t.Fatalf("value flag: %v", got)
	}
	t.Setenv("OFFERHOUND_BROWSER_FLAGS", "")
	if len(extraFlagsFromEnv()) != 0 {
		t.Fatal("empty env must yield no flags")
	}
}

func TestDisplayMissing(t *testing.T) {
	t.Setenv("DISPLAY", "")
	t.Setenv("WAYLAND_DISPLAY", "")
	_, missing := displayMissing()
	// on linux: missing; elsewhere: never missing
	if missing != (runtime.GOOS == "linux") {
		t.Fatalf("no-display detection: %v on %s", missing, runtime.GOOS)
	}
	t.Setenv("DISPLAY", ":0")
	if _, missing := displayMissing(); missing {
		t.Fatal("DISPLAY set => not missing")
	}
	t.Setenv("DISPLAY", "")
	t.Setenv("WAYLAND_DISPLAY", "wayland-0")
	if _, missing := displayMissing(); missing {
		t.Fatal("wayland counts as a display")
	}
}

func TestFindBrowserRespectsChromeBin(t *testing.T) {
	t.Setenv("CHROME_BIN", "/nonexistent/chromium")
	if bin, found := FindBrowser(); bin != "/nonexistent/chromium" || found {
		t.Fatalf("broken CHROME_BIN must be reported: %q %v", bin, found)
	}
	t.Setenv("CHROME_BIN", t.TempDir()+"/nope")
	checks := Preflight(PreflightOpts{})
	var saw bool
	for _, c := range checks {
		if c.Name == "chromium" && c.Status == "fail" {
			saw = true
		}
	}
	if !saw {
		t.Fatalf("broken CHROME_BIN must be a hard fail: %+v", checks)
	}
}

func TestHeadfulBankWithoutDisplayFails(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("display checks are linux-only")
	}
	t.Setenv("DISPLAY", "")
	t.Setenv("WAYLAND_DISPLAY", "")
	headful := false
	checks := Preflight(PreflightOpts{Banks: []config.Bank{
		{ID: "c1", Browser: config.BrowserConfig{Mode: "launch", Headless: &headful}},
	}})
	var saw bool
	for _, c := range checks {
		if c.Name == "display:c1" && c.Status == "fail" {
			saw = true
		}
	}
	if !saw {
		t.Fatalf("headful bank without display must fail preflight: %+v", checks)
	}
	if !PreflightFailed(checks) {
		t.Fatal("PreflightFailed must catch it")
	}
}

func TestPreflightNoChromiumWarns(t *testing.T) {
	t.Setenv("CHROME_BIN", "")
	// PATH without any browser name: warn, not fail (user may only serve)
	t.Setenv("PATH", t.TempDir())
	checks := Preflight(PreflightOpts{})
	var saw bool
	for _, c := range checks {
		if c.Name == "chromium" && c.Status == "warn" {
			saw = true
		}
	}
	if !saw {
		t.Fatalf("missing chromium should warn: %+v", checks)
	}
	if PreflightFailed(checks) {
		t.Fatal("a warn must not fail the whole preflight")
	}
}
