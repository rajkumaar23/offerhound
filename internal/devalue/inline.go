package devalue

import (
	"encoding/json"
	"fmt"
	"strings"
)

// DecodeInline decodes the INLINE devalue form emitted by Remix-style
// hydration payloads (e.g. window.__INITIAL_STATE__ blobs) where nothing is
// deduplicated into the top-level chunk array: every value is written
// literally inside its parent. Typed containers appear as
//
//	["~#iM", [k, v, k, v, ...]]   inline Map    -> object
//	["^ ",    k, v, k, v, ...]   object        -> object
//	["~#iL", [x, y, ...]]        inline List   -> array
//	["~#iS", [x, y, ...]]        inline Set    -> array (order preserved)
//
// Unknown "~"-marker arrays are decoded as plain lists of their remaining
// elements (lossy but never fatal). Unlike Decode, there are no index
// references — if the payload actually contains them this function returns
// them verbatim, which surfaces as a gjson miss rather than corruption.
func DecodeInline(raw []byte) (any, error) {
	var top any
	if err := json.Unmarshal(raw, &top); err != nil {
		return nil, fmt.Errorf("devalue(inline): not JSON: %w", err)
	}
	return inlineDecode(top), nil
}

func inlineDecode(x any) any {
	lst, ok := x.([]any)
	if !ok {
		return x
	}
	if len(lst) >= 1 {
		if marker, isStr := lst[0].(string); isStr && strings.HasPrefix(marker, "~") {
			switch marker {
			case "~#iM":
				return inlinePairs(lst[1:])
			case "~#iL", "~#iS":
				if len(lst) == 2 {
					return inlineDecode(lst[1])
				}
				return inlineList(lst[1:])
			default:
				return inlineList(lst[1:]) // unknown type tag: drop marker, keep body
			}
		}
		if marker, isStr := lst[0].(string); isStr && marker == "^ " {
			return inlinePairs(lst[1:])
		}
	}
	return inlineList(lst)
}

func inlineList(lst []any) []any {
	out := make([]any, 0, len(lst))
	for _, e := range lst {
		out = append(out, inlineDecode(e))
	}
	return out
}

func inlinePairs(body []any) any {
	// ["~#iM", [k,v,...]] hands us the wrapped form; unwrap once.
	if len(body) == 1 {
		if inner, isList := body[0].([]any); isList {
			body = inner
		}
	}
	d := make(map[string]any, len(body)/2)
	for i := 0; i+1 < len(body); i += 2 {
		key := fmt.Sprint(inlineDecode(body[i]))
		d[key] = inlineDecode(body[i+1])
	}
	return d
}
