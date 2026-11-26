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

// decoder rebuilds the flat-array graph. Memoizing resolved indices keeps a
// DAG (many references to one entry) linear instead of exponential, and the
// node budget bounds hostile payloads outright.
type decoder struct {
	arr      []any
	visiting []bool
	memo     map[int]any
	budget   int
}

const maxExpandedNodes = 1_000_000

func Decode(raw []byte) (any, error) {
	var arr []any
	if err := json.Unmarshal(raw, &arr); err != nil {
		return nil, fmt.Errorf("devalue: not a flat-array payload: %w", err)
	}
	if len(arr) == 0 {
		return nil, fmt.Errorf("devalue: empty payload")
	}
	d := &decoder{arr: arr, visiting: make([]bool, len(arr)), memo: map[int]any{}, budget: maxExpandedNodes}
	return d.resolve(0)
}

func (d *decoder) resolve(i int) (any, error) {
	if v, ok := d.memo[i]; ok {
		return v, nil
	}
	if i < 0 || i >= len(d.arr) {
		return nil, fmt.Errorf("devalue: index %d out of range", i)
	}
	if d.visiting[i] { // cycles are not expected; fail instead of looping
		return nil, fmt.Errorf("devalue: reference cycle at %d", i)
	}
	if d.budget <= 0 {
		return nil, fmt.Errorf("devalue: expansion budget (%d nodes) exhausted", maxExpandedNodes)
	}
	d.budget--
	d.visiting[i] = true
	defer func() { d.visiting[i] = false }()

	out, err := d.expand(i)
	if err != nil {
		return nil, err
	}
	d.memo[i] = out
	return out, nil
}

func (d *decoder) expand(i int) (any, error) {
	switch v := d.arr[i].(type) {
	case map[string]any:
		out := make(map[string]any, len(v))
		for k, w := range v {
			key, err := refIndex(k)
			if err != nil {
				return nil, fmt.Errorf("devalue: key at %d: %w", i, err)
			}
			keyVal, err := d.resolve(key)
			if err != nil {
				return nil, err
			}
			valRef, err := toIndex(w)
			if err != nil {
				return nil, fmt.Errorf("devalue: value at %d[%s]: %w", i, k, err)
			}
			val, err := d.resolve(valRef)
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
			elem, err := d.resolve(idx)
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
