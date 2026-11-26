package engine

import (
	"crypto/sha1"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/rajkumaar23/offerhound/internal/config"
	"github.com/rajkumaar23/offerhound/internal/offer"
	"github.com/tidwall/gjson"
)

// MapOffers converts a JSON document (raw bytes) into normalized offers
// using the extract spec. cardLabel is stamped on every offer.
func MapOffers(bankID, cardLabel string, doc []byte, ex *config.Extract, scrapedAt time.Time) ([]offer.Offer, error) {
	if ex == nil || ex.Items == "" {
		return nil, fmt.Errorf("extract: items path required")
	}
	items := getArr(doc, ex.Items)
	if !items.Exists() || !items.IsArray() {
		return nil, fmt.Errorf("extract: items path %q did not match an array", ex.Items)
	}
	status := offer.Status(ex.Status)
	if status == "" {
		status = offer.Eligible
	}
	var out []offer.Offer
	items.ForEach(func(_, item gjson.Result) bool {
		get := func(name string) (string, bool) {
			fm, ok := ex.Fields[name]
			if !ok {
				return "", false
			}
			if fm.Const != "" {
				return fm.Const, true
			}
			if fm.Path == "" {
				return fm.Default, fm.Default != ""
			}
			path := cleanPath(fm.Path)
			if fm.First {
				path += ".0"
			}
			v := item.Get(path)
			if !v.Exists() {
				return fm.Default, fm.Default != ""
			}
			s := v.String()
			switch fm.Transform {
			case "join":
				parts := []string{}
				v.ForEach(func(_, e gjson.Result) bool { parts = append(parts, e.String()); return true })
				s = strings.Join(parts, ", ")
			case "trim":
				s = strings.TrimSpace(s)
			case "beforeDash":
				// "Mulberry - Luxury Handbags & Accessories" => "Mulberry".
				// Some issuer offer portals stuff a category tag after the
				// brand;
				// the brand alone reads far better in list rows.
				if i := strings.Index(s, " - "); i > 0 {
					s = s[:i]
				}
			case "daysLeftToDate":
				if d := daysLeftToDate(s); d != "" {
					s = d
				} else {
					s = ""
				}
			case "expiresTextToDate":
				if d := expiresTextToDate(s); d != "" {
					s = d
				} else {
					s = ""
				}
			case "":
				// collapse newlines from button labels ("Spend $20,\n earn 500 miles")
				s = strings.Join(strings.Fields(s), " ")
			default:
				return "", false
			}
			return s, s != ""
		}
		merchant, ok := get("merchant")
		if !ok || merchant == "" {
			return true // skip malformed items silently; banks add junk entries
		}
		reward, _ := get("rewardText")
		title, hasTitle := get("title")
		if !hasTitle {
			title = reward
		}
		o := offer.Offer{
			Bank:      bankID,
			CardLabel: cardLabel,
			Merchant:  truncate(merchant, 120),
			Title:     truncate(title, 200),
			Status:    status,
			ScrapedAt: scrapedAt,
		}
		o.RewardText = truncate(mustGet(get, "rewardText"), 80)
		o.Description = truncate(mustGet(get, "description"), 500)
		o.Channel = mustGet(get, "channel")
		o.Notes = truncate(mustGet(get, "notes"), 4000)
		o.TermsURL = mustGet(get, "termsUrl")
		o.ExpiryDate = mustGet(get, "expiryDate")
		if d := o.ExpiryDate; d != "" {
			if _, err := time.Parse("2006-01-02", d); err != nil {
				o.ExpiryDate = ""
			}
		}
		o.Source = mustGet(get, "source")
		o.DetailURL = mustGet(get, "detailUrl")
		o.SourceID = mustGet(get, "sourceId") // raw issuer id for detail URL templating
		o.RewardValue = parseRewardValue(o.RewardText)
		o.ID = OfferID(o)
		out = append(out, o)
		return true
	})
	return out, nil
}

// OfferID derives a stable id from the business key.
func OfferID(o offer.Offer) string {
	key := strings.ToLower(fmt.Sprintf("%s|%s|%s|%s", o.Bank, o.CardLabel, o.Merchant, o.RewardText))
	seed := sha1.Sum([]byte(key))
	return fmt.Sprintf("%s-%x", o.Bank, seed[:10])
}

var (
	multiX      = regexp.MustCompile(`(?i)(\d+(?:\.\d+)?)\s*[x×]`)
	dollar      = regexp.MustCompile(`\$([\d,]+(?:\.\d+)?)`)
	percent     = regexp.MustCompile(`(\d+(?:\.\d+)?)\s*%`)
	daysLeft    = regexp.MustCompile(`(?i)(\d+)\s*days?\s*left`)
	expiresText = regexp.MustCompile(`(?i)expires\s+(\d{1,2})/(\d{1,2})/(\d{2,4})\s*$`)
)

// parseRewardValue extracts a comparable magnitude: Nx → N, $X → X, N% → N.
func parseRewardValue(s string) float64 {
	if m := multiX.FindStringSubmatch(s); m != nil {
		v, _ := strconv.ParseFloat(m[1], 64)
		return v
	}
	if m := dollar.FindStringSubmatch(s); m != nil {
		v, _ := strconv.ParseFloat(strings.ReplaceAll(m[1], ",", ""), 64)
		return v
	}
	if m := percent.FindStringSubmatch(s); m != nil {
		v, _ := strconv.ParseFloat(m[1], 64)
		return v
	}
	return 0
}

// daysLeftToDate turns "7 days left" into YYYY-MM-DD from today.
func daysLeftToDate(s string) string {
	m := daysLeft.FindStringSubmatch(s)
	if m == nil {
		return ""
	}
	n, _ := strconv.Atoi(m[1])
	return time.Now().UTC().AddDate(0, 0, n).Format("2006-01-02")
}

// expiresTextToDate turns issuer-style "Expires 12/31/26" (M/D/YY or M/D/YYYY)
// into YYYY-MM-DD. Two-digit years are 20YY. Anything else yields "".
func expiresTextToDate(s string) string {
	m := expiresText.FindStringSubmatch(strings.TrimSpace(s))
	if m == nil {
		return ""
	}
	mo, err1 := strconv.Atoi(m[1])
	d, err2 := strconv.Atoi(m[2])
	y, err3 := strconv.Atoi(m[3])
	if err1 != nil || err2 != nil || err3 != nil || mo < 1 || mo > 12 || d < 1 || d > 31 {
		return ""
	}
	if y < 100 {
		y += 2000
	}
	t := time.Date(y, time.Month(mo), d, 0, 0, 0, 0, time.UTC)
	if t.Month() != time.Month(mo) || t.Day() != d {
		return "" // rolled over => invalid calendar date
	}
	return t.Format("2006-01-02")
}

func mustGet(get func(string) (string, bool), name string) string {
	v, _ := get(name)
	return v
}

func cleanPath(p string) string {
	p = strings.TrimPrefix(p, "$.")
	p = strings.TrimPrefix(p, "$")
	p = strings.TrimSuffix(p, "[*]")
	return strings.TrimSuffix(p, ".")
}

// getArr resolves a (possibly root) array path. gjson's Get has no
// wildcard-path support, so "data[*]" collapses to "data" and "$[*]"/""
// resolves to the document root. A trailing ".*" additionally accepts an
// OBJECT at the parent path and returns its values as an array — gjson
// wildcards never do that (verified: "obj.*", "obj.#", "obj.*.k" all fail
// to yield arrays), and map-shaped collections like issuer products-list
// registries are
// common inside hydration blobs.
func getArr(doc []byte, path string) gjson.Result {
	p := cleanPath(path)
	if p == "" {
		return gjson.ParseBytes(doc)
	}
	r := gjson.GetBytes(doc, p)
	if !r.IsArray() && strings.HasSuffix(p, ".*") {
		if obj := gjson.GetBytes(doc, strings.TrimSuffix(p, ".*")); obj.IsObject() {
			var vals []string
			obj.ForEach(func(_, v gjson.Result) bool {
				vals = append(vals, v.Raw)
				return true
			})
			return gjson.Parse("[" + strings.Join(vals, ",") + "]")
		}
	}
	return r
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}
