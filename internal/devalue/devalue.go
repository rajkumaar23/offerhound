// Package devalue decodes the flat-array wire format produced by
// SvelteKit's `data` endpoints.
//
// The payload is a single JSON array; object entries written as {"_N": M}
// are index references into that same array (keys reference values, both
// resolved recursively).
package devalue

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
)

// Decode parses a flat-array devalue payload into plain Go values
// (map[string]any / []any / primitives). The top-level value is element 0.
func Decode(raw []byte) (any, error) {
	var arr []any
	if err := json.Unmarshal(raw, &arr); err != nil {
		return nil, fmt.Errorf("devalue: not a flat-array payload: %w", err)
	}
	if len(arr) == 0 {
		return nil, fmt.Errorf("devalue: empty payload")
	}
	return resolve(arr, 0, make([]bool, len(arr)))
}

func resolve(arr []any, i int, visiting []bool) (any, error) {
	if i < 0 || i >= len(arr) {
		return nil, fmt.Errorf("devalue: index %d out of range", i)
	}
	if visiting[i] { // cycles are not expected; fail instead of looping
		return nil, fmt.Errorf("devalue: reference cycle at %d", i)
	}
	visiting[i] = true
	defer func() { visiting[i] = false }()

	switch v := arr[i].(type) {
	case map[string]any:
		out := make(map[string]any, len(v))
		for k, w := range v {
			key, err := refIndex(k)
			if err != nil {
				return nil, fmt.Errorf("devalue: key at %d: %w", i, err)
			}
			keyVal, err := resolve(arr, key, visiting)
			if err != nil {
				return nil, err
			}
			valRef, err := toIndex(w)
			if err != nil {
				return nil, fmt.Errorf("devalue: value at %d[%s]: %w", i, k, err)
			}
			val, err := resolve(arr, valRef, visiting)
			if err != nil {
				return nil, err
			}
			out[fmt.Sprint(keyVal)] = val
		}
		return out, nil
	case []any:
		out := make([]any, 0, len(v))
		for _, x := range v {
			idx, err := toIndex(x)
			if err != nil {
				return nil, fmt.Errorf("devalue: list at %d: %w", i, err)
			}
			elem, err := resolve(arr, idx, visiting)
			if err != nil {
				return nil, err
			}
			out = append(out, elem)
		}
		return out, nil
	default:
		return v, nil // primitives at a referenced slot
	}
}

func refIndex(k string) (int, error) {
	if !strings.HasPrefix(k, "_") {
		return 0, fmt.Errorf("object keys must be _N references, got %q", k)
	}
	return strconv.Atoi(k[1:])
}

func toIndex(v any) (int, error) {
	f, ok := v.(float64)
	if !ok {
		return 0, fmt.Errorf("expected index, got %T", v)
	}
	return int(f), nil
}
