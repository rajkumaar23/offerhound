// Package config defines the offerhound bank-automation DSL.
//
// A Bank config is a YAML file describing how to authenticate against one
// issuer and where its personalized card offers live. Shape: tagged
// polymorphic steps validated up front so most config mistakes fail at
// `offerhound verify`, not at 3am sync time.
package config

import (
	"fmt"
	"net/url"
	"regexp"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// BrowserConfig selects how the engine drives Chrome.
type BrowserConfig struct {
	Mode       string `yaml:"mode"`     // launch (default) | attach
	Headless   *bool  `yaml:"headless"` // launch mode; default true
	CDPURL     string `yaml:"cdpUrl"`   // attach mode: http://host:port
	ProfileDir string `yaml:"profileDir"`
	UserAgent  string `yaml:"userAgent"`
	Locale     string `yaml:"locale"`
}

// RateLimit keeps us on the good side of issuer throttling.
type RateLimit struct {
	DelayBetween   Duration `yaml:"delayBetween"`
	RequestsPerRun int      `yaml:"requestsPerRun"`
}

// Credentials maps credential names to references (env://NAME,
// keyring://service/name, file://path). Literals are rejected — secrets
// never belong in a YAML config.
type Credentials map[string]string

// Probe is a cheap signed-in check; when it succeeds the login flow is
// skipped entirely (session persistence does the work).
type Probe struct {
	FetchAPI string `yaml:"fetchApi"`
	OKStatus []int  `yaml:"okStatus"`
	HasPath  string `yaml:"hasPath"` // gjson path (no $ prefix; root-array fields are "0.field") that must exist & be non-null
}

// Login is the auth flow: probe + steps run only when signed out.
type Login struct {
	Probe *Probe `yaml:"probe"`
	Steps []Step `yaml:"steps"`
}

// EachSpec loops a step over a JSON array saved by a previous step.
type EachSpec struct {
	From  string `yaml:"from"`  // saveAs key of a previous fetchApi
	Items string `yaml:"items"` // gjson path, e.g. "$[*]"
	As    string `yaml:"as"`    // template variable name
}

// Paginate follows cursor-paginated JSON endpoints.
type Paginate struct {
	CursorParam string `yaml:"cursorParam"` // query param carrying the cursor
	CursorPath  string `yaml:"cursorPath"`  // gjson path in response for next cursor
	MaxPages    int    `yaml:"maxPages"`
}

// FieldMap maps one offer attribute out of JSON. Shorthand: a bare string
// is the path.
type FieldMap struct {
	Path      string `yaml:"path"`
	Const     string `yaml:"const"`
	First     bool   `yaml:"first"` // take element 0 of an array path
	Transform string `yaml:"transform"`
	Default   string `yaml:"default"`
}

func (f *FieldMap) UnmarshalYAML(value *yaml.Node) error {
	var s string
	if err := value.Decode(&s); err == nil {
		f.Path = s
		return nil
	}
	type plain FieldMap
	return value.Decode((*plain)(f))
}

// Extract maps a saved JSON document into normalized offers.
type Extract struct {
	Items  string              `yaml:"items"`  // gjson path to the item array
	Fields map[string]FieldMap `yaml:"fields"` // offer field -> mapping
	Status string              `yaml:"status"` // eligible | enrolled
}

// CardsSpec extracts per-card labels for UI chips.
type CardsSpec struct {
	Each      string            `yaml:"each"`
	CardLabel string            `yaml:"cardLabel"` // text/template over item
	Extra     map[string]string `yaml:"extra"`
}

// WaitSpec is a selector + timeout (timeouts are mandatory: an unbounded
// wait can hang a run forever).
type WaitSpec struct {
	Selector string   `yaml:"selector"`
	JSPath   string   `yaml:"js_path"`
	Timeout  Duration `yaml:"timeout"`
}

// ClickSpec: trusted=true dispatches real CDP mouse events (React popups
// opened via window.open ignore synthetic clicks).
type ClickSpec struct {
	Selector string   `yaml:"selector"`
	JSPath   string   `yaml:"js_path"`
	Trusted  bool     `yaml:"trusted"`
	Timeout  Duration `yaml:"timeout"`
}

// FillSpec types a value using the native value setter + input/change
// events (React/Angular compatible).
type FillSpec struct {
	Selector  string `yaml:"selector"`
	JSPath    string `yaml:"js_path"`
	Value     string `yaml:"value"`
	ValueFrom string `yaml:"valueFrom"` // credential.<name>
	Submit    string `yaml:"submit"`    // selector to click after filling
}

// HumanSpec pauses a run for a person (SMS/push/captcha). Human-paced steps
// get generous timeouts and never auto-guess values.
type HumanSpec struct {
	When    *WaitSpec `yaml:"when"` // fire only if this appears; nil => always
	Prompt  string    `yaml:"prompt"`
	Fill    *FillSpec `yaml:"fill"`
	Always  bool      `yaml:"always"`
	Timeout Duration  `yaml:"timeout"`
}

// FetchAPISpec replays an in-tab fetch with cookies — the durable way to
// read JSON from modern SPA frontends. Authored flattened into a Step.
type FetchAPISpec struct {
	Name     string            `yaml:"name"`
	URL      string            `yaml:"fetchApi"` // template: /feed/{{urlquery .acct.id}}
	SaveAs   string            `yaml:"saveAs"`
	Devalue  bool              `yaml:"devalue"` // decode SvelteKit flat-array wire format
	Headers  map[string]string `yaml:"headers"`
	Paginate *Paginate         `yaml:"paginate"`
	Each     *EachSpec         `yaml:"each"`
	Extract  *Extract          `yaml:"extract"`
	Cards    *CardsSpec        `yaml:"cards"`
}

// StepType enumerates the DSL vocabulary.
type StepType string

const (
	StepNavigate     StepType = "navigate"
	StepWaitSelector StepType = "waitSelector"
	StepWaitNotVis   StepType = "waitNotVisible"
	StepClick        StepType = "click"
	StepFill         StepType = "fill"
	StepSleep        StepType = "sleep"
	StepReload       StepType = "reload"
	StepFetchAPI     StepType = "fetchApi"
	StepHuman        StepType = "human"
)

// Step is one tagged union member. Exactly one kind should be set;
// fetchApi steps carry their extract/each/paginate inline (flattened).
type Step struct {
	Navigate     *string       `yaml:"-"`
	WaitSelector *WaitSpec     `yaml:"-"`
	WaitNotVis   *WaitSpec     `yaml:"-"`
	Click        *ClickSpec    `yaml:"-"`
	Fill         *FillSpec     `yaml:"-"`
	Sleep        Duration      `yaml:"-"`
	Reload       *bool         `yaml:"-"`
	FetchAPI     *FetchAPISpec `yaml:"-"`
	Human        *HumanSpec    `yaml:"-"`
}

// stepYAML is the flat wire form; fetchApi fields live at the same level as
// the step tags for config ergonomics.
type stepYAML struct {
	Navigate     *string           `yaml:"navigate"`
	WaitSelector *WaitSpec         `yaml:"waitSelector"`
	WaitNotVis   *WaitSpec         `yaml:"waitNotVisible"`
	Click        *ClickSpec        `yaml:"click"`
	Fill         *FillSpec         `yaml:"fill"`
	Sleep        Duration          `yaml:"sleep"`
	Reload       *bool             `yaml:"reload"`
	Name         string            `yaml:"name"`
	FetchAPI     string            `yaml:"fetchApi"`
	SaveAs       string            `yaml:"saveAs"`
	Devalue      bool              `yaml:"devalue"`
	Headers      map[string]string `yaml:"headers"`
	Paginate     *Paginate         `yaml:"paginate"`
	Each         *EachSpec         `yaml:"each"`
	Extract      *Extract          `yaml:"extract"`
	Cards        *CardsSpec        `yaml:"cards"`
	Human        *HumanSpec        `yaml:"human"`
}

func (s *Step) UnmarshalYAML(value *yaml.Node) error {
	var y stepYAML
	if err := value.Decode(&y); err != nil {
		return err
	}
	s.Navigate, s.WaitSelector, s.WaitNotVis = y.Navigate, y.WaitSelector, y.WaitNotVis
	s.Click, s.Fill, s.Sleep, s.Reload = y.Click, y.Fill, y.Sleep, y.Reload
	s.Human = y.Human
	if y.FetchAPI != "" || y.SaveAs != "" || y.Each != nil || y.Extract != nil {
		if y.FetchAPI == "" {
			return fmt.Errorf("fetchApi steps require fetchApi URL")
		}
		s.FetchAPI = &FetchAPISpec{
			Name: y.Name, URL: y.FetchAPI, SaveAs: y.SaveAs, Devalue: y.Devalue,
			Headers: y.Headers, Paginate: y.Paginate, Each: y.Each,
			Extract: y.Extract, Cards: y.Cards,
		}
	}
	return nil
}

// Types returns the kinds present on this step.
func (s *Step) Types() []StepType {
	var out []StepType
	add := func(t StepType, ok bool) {
		if ok {
			out = append(out, t)
		}
	}
	add(StepNavigate, s.Navigate != nil)
	add(StepWaitSelector, s.WaitSelector != nil)
	add(StepWaitNotVis, s.WaitNotVis != nil)
	add(StepClick, s.Click != nil)
	add(StepFill, s.Fill != nil)
	add(StepSleep, s.Sleep > 0)
	add(StepReload, s.Reload != nil)
	add(StepFetchAPI, s.FetchAPI != nil)
	add(StepHuman, s.Human != nil)
	return out
}

// Bank is a complete bank configuration document.
type Bank struct {
	ID          string        `yaml:"id"`
	Name        string        `yaml:"name"`
	BaseURL     string        `yaml:"baseUrl"`
	Schema      int           `yaml:"schema"` // DSL version, currently 1
	Browser     BrowserConfig `yaml:"browser"`
	RateLimit   RateLimit     `yaml:"rateLimit"`
	Credentials Credentials   `yaml:"credentials"`
	Login       *Login        `yaml:"login"`
	Offers      []Step        `yaml:"offers"`

	path string // source file, for error messages
}

// Path returns the file the config was loaded from (if any).
func (b *Bank) Path() string { return b.path }

var (
	idRe    = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,31}$`)
	credRef = regexp.MustCompile(`^(env|keyring|file)://[A-Za-z0-9_./-]+$`)
)

// Validate enforces DSL invariants, including the read-only rule: offer
// flows may not click, type, or pause for humans — scraping only.
func (b *Bank) Validate() error {
	berr := func(f string, a ...any) error {
		return fmt.Errorf("bank %q (%s): %s", b.ID, b.path, fmt.Sprintf(f, a...))
	}
	if !idRe.MatchString(b.ID) {
		return berr("id must match ^[a-z0-9][a-z0-9_-]{0,31}$")
	}
	if b.Name == "" {
		return berr("name required")
	}
	if b.Schema != 0 && b.Schema != 1 {
		return berr("unsupported schema version %d", b.Schema)
	}
	u, e := url.Parse(b.BaseURL)
	if e != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return berr("baseUrl must be an absolute http(s) URL")
	}
	switch b.Browser.Mode {
	case "", "launch":
	case "attach":
		if b.Browser.CDPURL == "" {
			return berr("browser.mode=attach requires browser.cdpUrl")
		}
	default:
		return berr("browser.mode must be launch|attach, got %q", b.Browser.Mode)
	}
	for name, ref := range b.Credentials {
		if !credRef.MatchString(ref) {
			return berr("credential %q must be a reference (env://NAME, keyring://svc/name, file://path), not a literal", name)
		}
	}
	if b.Login != nil {
		for i, s := range b.Login.Steps {
			if e := s.validate(); e != nil {
				return berr("login.steps[%d]: %v", i, e)
			}
			if e := s.checkCredentialRefs(b.Credentials); e != nil {
				return berr("login.steps[%d]: %v", i, e)
			}
		}
	}
	if len(b.Offers) == 0 {
		return berr("at least one offers step required")
	}
	saved := map[string]bool{}
	for i, s := range b.Offers {
		if e := s.validate(); e != nil {
			return berr("offers[%d]: %v", i, e)
		}
		for _, t := range s.Types() {
			switch t {
			case StepClick, StepFill, StepHuman, StepNavigate:
				return berr("offers[%d]: step type %q forbidden in offers flow (read-only rule; navigation is automatic)", i, t)
			}
		}
		if f := s.FetchAPI; f != nil {
			if f.Each != nil {
				if !saved[f.Each.From] {
					return berr("offers[%d]: each.from %q not saved by an earlier step", i, f.Each.From)
				}
			}
			if f.Each == nil && f.Extract != nil {
				// extracting the document this very step fetched is fine
				f.Each = nil
			}
			if f.SaveAs != "" {
				saved[f.SaveAs] = true
			}
			if f.Extract != nil && f.Extract.Items == "" {
				return berr("offers[%d]: extract.items required", i)
			}
			if f.Extract != nil && f.Extract.Status != "" &&
				f.Extract.Status != "eligible" && f.Extract.Status != "enrolled" {
				return berr("offers[%d]: extract.status must be eligible|enrolled", i)
			}
		}
	}
	return nil
}

func (s *Step) validate() error {
	ts := s.Types()
	if len(ts) == 0 {
		return fmt.Errorf("empty step")
	}
	if len(ts) > 1 {
		return fmt.Errorf("exactly one step kind allowed, got %v", ts)
	}
	if w := s.WaitSelector; w != nil {
		if w.Selector == "" && w.JSPath == "" {
			return fmt.Errorf("waitSelector needs selector or js_path")
		}
		if w.Timeout == 0 {
			return fmt.Errorf("waitSelector needs an explicit timeout")
		}
	}
	if w := s.WaitNotVis; w != nil {
		if w.Selector == "" && w.JSPath == "" {
			return fmt.Errorf("waitNotVisible needs selector or js_path")
		}
		if w.Timeout == 0 {
			return fmt.Errorf("waitNotVisible needs an explicit timeout")
		}
	}
	if c := s.Click; c != nil && c.Selector == "" && c.JSPath == "" {
		return fmt.Errorf("click needs selector or js_path")
	}
	if f := s.Fill; f != nil {
		if f.Selector == "" && f.JSPath == "" {
			return fmt.Errorf("fill needs selector or js_path")
		}
		if f.Value == "" && f.ValueFrom == "" {
			return fmt.Errorf("fill needs value or valueFrom")
		}
	}
	if f := s.FetchAPI; f != nil {
		if f.Paginate != nil && (f.Paginate.CursorParam == "" || f.Paginate.CursorPath == "") {
			return fmt.Errorf("paginate needs cursorParam and cursorPath")
		}
		if f.Each != nil && (f.Each.From == "" || f.Each.Items == "" || f.Each.As == "") {
			return fmt.Errorf("each needs from, items, as")
		}
	}
	if h := s.Human; h != nil {
		if h.Prompt == "" {
			return fmt.Errorf("human needs a prompt shown to the user")
		}
		if h.Timeout == 0 {
			h.Timeout = Duration(10 * time.Minute)
		}
	}
	return nil
}

func (s *Step) checkCredentialRefs(creds Credentials) error {
	if s.Fill == nil || s.Fill.ValueFrom == "" {
		return nil
	}
	name := strings.TrimPrefix(s.Fill.ValueFrom, "credential.")
	if name == s.Fill.ValueFrom {
		return fmt.Errorf("valueFrom must be credential.<name>, got %q", s.Fill.ValueFrom)
	}
	if _, ok := creds[name]; !ok {
		return fmt.Errorf("valueFrom references unknown credential %q", name)
	}
	return nil
}

// Duration is a time.Duration that YAMLs as "1200ms".
type Duration time.Duration

func (d *Duration) UnmarshalYAML(value *yaml.Node) error {
	var s string
	if err := value.Decode(&s); err != nil {
		return err
	}
	parsed, err := time.ParseDuration(s)
	if err != nil {
		return fmt.Errorf("bad duration %q: %w", s, err)
	}
	*d = Duration(parsed)
	return nil
}

func (d Duration) Dur() time.Duration { return time.Duration(d) }
