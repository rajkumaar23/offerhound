package engine

import (
	"strings"
	"testing"

	"github.com/rajkumaar23/offerhound/internal/config"
	"github.com/tidwall/gjson"
)

const amexishHTML = `<!DOCTYPE html><html><head>
<script id="other">var x = 1;</script>
</head><body>
<script id="initial-state" nonce="abc-123">
  window.__webpack_public_path__ = "https://cdn.example/";
  window.__INITIAL_STATE__ = "[\"~#iM\", [\"modules\", [\"~#iM\", [\"axp-loyalty-root\", [\"~#iM\", [\"result\", [\"~#iM\", [\"offers\", [\"^ \", \"title\", \"Target Run\", \"enrollmentDetails\", [\"^ \", \"status\", \"NOT_ENROLLED\"], \"expiration\", [\"^ \", \"text\", \"Expires 12/31/26\"]]]]]]]]]]";
  window.__render_mode__ = 'hydrate';
</script>
</body></html>`

func TestDecodeBodyInlineInitialState(t *testing.T) {
	spec := &config.DecodeSpec{
		ScriptID:   "initial-state",
		Assignment: "window.__INITIAL_STATE__",
		Format:     "inlineDevalue",
	}
	doc, err := decodeBody(amexishHTML, spec, false)
	if err != nil {
		t.Fatal(err)
	}
	offer := gjson.Get(doc, "modules.axp-loyalty-root.result.offers")
	if !offer.Exists() {
		t.Fatalf("path missing in %s", doc[:min(len(doc), 200)])
	}
	if got := offer.Get("title").String(); got != "Target Run" {
		t.Fatalf("title = %q", got)
	}
	if got := offer.Get("expiration.text").String(); got != "Expires 12/31/26" {
		t.Fatalf("expiration = %q", got)
	}
}

func TestDecodeBodyScriptMissing(t *testing.T) {
	spec := &config.DecodeSpec{ScriptID: "nope", Format: "inlineDevalue"}
	if _, err := decodeBody(amexishHTML, spec, false); err == nil {
		t.Fatal("expected error for missing script")
	}
}

func TestDecodeBodyAssignmentMissing(t *testing.T) {
	spec := &config.DecodeSpec{ScriptID: "initial-state", Assignment: "window.__NOT_THERE__", Format: "inlineDevalue"}
	if _, err := decodeBody(amexishHTML, spec, false); err == nil {
		t.Fatal("expected error for missing assignment")
	}
}

func TestDecodeBodyLegacyDevalueKept(t *testing.T) {
	// SvelteKit flat form: root is element 0; object {"_1":2} means
	// key=arr[1], value=arr[2].
	raw := `[{"_1":2},"merchant",3]`
	doc, err := decodeBody(raw, nil, true)
	if err != nil {
		t.Fatal(err)
	}
	if gjson.Get(doc, "merchant").Int() != 3 {
		t.Fatalf("legacy devalue path broken: %s", doc)
	}
}

func TestDecodeBodyNoOpPassThrough(t *testing.T) {
	doc, err := decodeBody(`{"a":1}`, nil, false)
	if err != nil || !strings.Contains(doc, `"a":1`) {
		t.Fatalf("passthrough broke: %q %v", doc, err)
	}
}

func TestDecodeBodySingleQuotedLiteralFails(t *testing.T) {
	html := `<script id="s">window.X = 'not-json-escaped';</script>`
	spec := &config.DecodeSpec{ScriptID: "s", Assignment: "window.X", Format: "inlineDevalue"}
	// single quotes are acceptable delimiters; content must decode as a JSON
	// string. Plain text still decodes fine as a string, so check the actual
	// payload is usable...
	doc, err := decodeBody(html, spec, false)
	if err == nil {
		t.Fatalf("expected non-array payload to fail inline decode, got %q", doc)
	}
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
