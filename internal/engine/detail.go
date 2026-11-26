package engine

import (
	"context"
	"regexp"
	"strings"
	"time"

	"github.com/rajkumaar23/offerhound/internal/config"
	"github.com/rajkumaar23/offerhound/internal/offer"
	"github.com/tidwall/gjson"
)

// enrichDetails fetches detail.Document per distinct offer id and fills
// Notes/TermsURL. A failed fetch only skips enrichment for that offer —
// the list-level offer data already stands on its own.
func (r *Runner) enrichDetails(ctx context.Context, f *config.FetchAPISpec, vars map[string]any,
	offers []offer.Offer, lim *limiter) {
	byID := map[string][]*offer.Offer{}
	for i := range offers {
		id := offers[i].ID
		if id == "" {
			id = offers[i].Merchant + "|" + offers[i].Title
		}
		byID[id] = append(byID[id], &offers[i])
	}
	for id, group := range byID {
		dvars := make(map[string]any, len(vars)+1)
		for k, v := range vars {
			dvars[k] = v
		}
		o := group[0]
		dvars["offer"] = map[string]string{"id": id, "sourceId": o.SourceID, "merchant": o.Merchant, "title": o.Title}
		u, err := renderTemplate(f.Detail.URL, dvars)
		if err != nil {
			r.logf("detail url template (%s): %v", id, err)
			continue
		}
		if err := lim.acquire(); err != nil {
			return
		}
		body, status, retryAfter, err := r.fetch(ctx, u, f.Detail.Headers)
		if status == 429 {
			// one polite retry — the portals throttle bursty detail callers
			wait := 15 * time.Second
			if d, e := time.ParseDuration(retryAfter + "s"); e == nil && d > 0 {
				wait = d
			}
			select {
			case <-ctx.Done():
				return
			case <-time.After(wait):
			}
			body, status, _, err = r.fetch(ctx, u, f.Detail.Headers)
		}
		if err != nil || status < 200 || status >= 300 {
			r.logf("detail fetch skipped (%s): status=%d err=%v", id, status, err)
			continue
		}
		doc, derr := decodeBody(body, f.Detail.Decode, false)
		if derr != nil {
			r.logf("detail decode skipped (%s): %v", id, derr)
			continue
		}
		notes := buildDetailNotes([]byte(doc), f.Detail.Notes)
		for _, o := range group {
			if notes != "" {
				if o.Notes != "" {
					o.Notes += "\n\n"
				}
				o.Notes += notes
			}
			if f.Detail.TermsPath != "" && o.TermsURL == "" {
				if v := gjson.GetBytes([]byte(doc), cleanPath(f.Detail.TermsPath)); v.Exists() {
					o.TermsURL = v.String()
				}
			}
		}
	}
}

// buildDetailNotes renders the configured note blocks from a per-offer
// detail document into one notes string. Empty blocks are dropped; blocks
// are separated by blank lines.
func buildDetailNotes(doc []byte, blocks []config.NoteBlock) string {
	var out []string
	for _, b := range blocks {
		if b.Root == "" {
			continue
		}
		arr := getArr(doc, b.Root)
		if !arr.Exists() || !arr.IsArray() {
			continue
		}
		var lines []string
		arr.ForEach(func(_, el gjson.Result) bool {
			prefix := ""
			if b.PrefixFrom != "" {
				prefix = strings.Join(strings.Fields(el.Get(cleanPath(b.PrefixFrom)).String()), " ")
			}
			if b.Skip {
				if prefix != "" {
					lines = append(lines, "• "+prefix)
				}
				return true
			}
			var body []string
			t := el.Get(cleanPath(b.TextFrom))
			if t.IsArray() {
				t.ForEach(func(_, s gjson.Result) bool {
					if v := cleanMarkdown(s.String()); v != "" {
						body = append(body, v)
					}
					return true
				})
			} else if v := cleanMarkdown(t.String()); v != "" {
				body = append(body, v)
			}
			switch {
			case prefix != "" && len(body) > 0:
				lines = append(lines, prefix+": "+strings.Join(body, " / "))
			case prefix != "":
				lines = append(lines, prefix)
			case len(body) > 0:
				lines = append(lines, body...)
			}
			return true
		})
		if len(lines) > 0 {
			out = append(out, strings.Join(lines, "\n"))
		}
	}
	return strings.Join(out, "\n\n")
}

var (
	reBold  = regexp.MustCompile(`\*\*(.+?)\*\*`)
	reBold2 = regexp.MustCompile(`__(.+?)__`)
	reLink  = regexp.MustCompile(`\[([^\]]+)\]\(([^)]+)\)`)
	reCode  = regexp.MustCompile("`([^`]+)`")
)

// cleanMarkdown degrades the markdown the portals sprinkle in their terms
// into readable plain text (the UI renders notes as plain text).
func cleanMarkdown(s string) string {
	s = strings.TrimSpace(s)
	s = reBold.ReplaceAllString(s, "$1")
	s = reBold2.ReplaceAllString(s, "$1")
	s = reLink.ReplaceAllString(s, "$1 ($2)")
	s = reCode.ReplaceAllString(s, "$1")
	s = strings.Join(strings.Fields(s), " ")
	return strings.TrimSpace(s)
}
