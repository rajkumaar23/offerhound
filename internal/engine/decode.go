package engine

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"

	"github.com/rajkumaar23/offerhound/internal/config"
	"github.com/rajkumaar23/offerhound/internal/devalue"
)

// decodeBody turns a raw HTTP body into the JSON document that saveAs /
// extract operate on. nil spec + devalue=false is a pass-through; nil spec
// + devalue=true keeps the legacy SvelteKit flat-array path.
func decodeBody(body string, spec *config.DecodeSpec, legacyDevalue bool) (string, error) {
	if spec == nil {
		if !legacyDevalue {
			return body, nil
		}
		decoded, err := devalue.Decode([]byte(body))
		if err != nil {
			return "", err
		}
		return marshalDoc(decoded)
	}
	raw := body
	var err error
	if spec.ScriptID != "" {
		if raw, err = htmlScriptBody(raw, spec.ScriptID); err != nil {
			return "", fmt.Errorf("decode script %q: %w", spec.ScriptID, err)
		}
	}
	if spec.Assignment != "" {
		if raw, err = jsAssignmentString(raw, spec.Assignment); err != nil {
			return "", fmt.Errorf("decode assignment %q: %w", spec.Assignment, err)
		}
	}
	var decoded any
	switch spec.Format {
	case "devalue":
		decoded, err = devalue.Decode([]byte(raw))
	case "inlineDevalue":
		decoded, err = devalue.DecodeInline([]byte(raw))
	default:
		return "", fmt.Errorf("decode: unknown format %q", spec.Format)
	}
	if err != nil {
		return "", err
	}
	return marshalDoc(decoded)
}

func marshalDoc(v any) (string, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return "", fmt.Errorf("decode: re-marshal: %w", err)
	}
	return string(b), nil
}

// htmlScriptBody returns the content of <script id="id">…</script>. The id
// attribute may appear among other attributes (nonce, type, ...).
func htmlScriptBody(html, id string) (string, error) {
	marker := `id="` + id + `"`
	i := strings.Index(html, marker)
	if i < 0 {
		return "", fmt.Errorf("script id %q not found", id)
	}
	open := strings.LastIndex(html[:i], "<script")
	if open < 0 {
		return "", fmt.Errorf("no <script> tag before id %q", id)
	}
	tagEnd := strings.Index(html[i:], ">")
	if tagEnd < 0 {
		return "", fmt.Errorf("unterminated <script> tag for id %q", id)
	}
	contentStart := i + tagEnd + 1
	end := strings.Index(html[contentStart:], "</script>")
	if end < 0 {
		return "", fmt.Errorf("unterminated </script> for id %q", id)
	}
	return html[contentStart : contentStart+end], nil
}

// jsAssignmentString extracts the string value assigned to
// `window.NAME = "..."` (or bare `NAME = '...'`). The literal must be
// JSON-compatible (double-quoted, standard escapes), which holds for
// bundle-emitted hydration assignments; JS-only escapes fail loudly rather
// than corrupting the payload.
func jsAssignmentString(src, assignment string) (string, error) {
	name := strings.TrimPrefix(assignment, "window.")
	re := regexp.MustCompile(`(?m)(?:^|[;\s{])(?:window\.)?\Q` + name + `\E\s*=\s*`)
	loc := re.FindStringIndex(src)
	if loc == nil {
		return "", fmt.Errorf("assignment %q not found", assignment)
	}
	rest := src[loc[1]:]
	if rest == "" || (rest[0] != '"' && rest[0] != '\'') {
		return "", fmt.Errorf("assignment %q: expected a string literal", assignment)
	}
	end := findJSLiteralEnd(rest)
	if end < 0 {
		return "", fmt.Errorf("assignment %q: unterminated string literal", assignment)
	}
	var s string
	if err := json.Unmarshal([]byte(rest[:end]), &s); err != nil {
		return "", fmt.Errorf("assignment %q: %w", assignment, err)
	}
	return s, nil
}

// findJSLiteralEnd returns the index just past the closing quote of a
// quote-delimited literal starting at position 0, honoring backslash escapes.
func findJSLiteralEnd(s string) int {
	q := s[0]
	for i := 1; i < len(s); i++ {
		switch s[i] {
		case '\\':
			i++ // skip escaped char
		case q:
			return i + 1
		}
	}
	return -1
}
