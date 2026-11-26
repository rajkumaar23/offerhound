package devalue

import (
	"encoding/json"
	"reflect"
	"testing"
	"time"
)

func TestDecodeFlatArray(t *testing.T) {
	// {"meta": {...}, "success": true, "offers": [ {"merchant": "Chewy"} ]}
	raw := `[{"_1":2},"meta",{"_3":4,"_5":6},"success",true,"offers",[7],` +
		`{"_8":9},` +
		`"merchant","Chewy"]`
	got, err := Decode([]byte(raw))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	want := map[string]any{
		"meta": map[string]any{
			"success": true,
			"offers":  []any{map[string]any{"merchant": "Chewy"}},
		},
	}
	if !reflect.DeepEqual(got, want) {
		g, _ := json.Marshal(got)
		t.Fatalf("got %s", g)
	}
}

func TestRejectNonArray(t *testing.T) {
	if _, err := Decode([]byte(`{"a":1}`)); err == nil {
		t.Fatal("expected error for object payload")
	}
}

func TestDecodeDAGNotExponential(t *testing.T) {
	// arr[0]=[1,1]; each arr[k]=[k+1,k+1] for k<n; arr[n]=[]. Without
	// memoization resolving index 0 costs 2^n node visits; memoized it is
	// linear. n=26 would take forever otherwise.
	const n = 26
	arr := make([]any, n+1)
	for k := 0; k < n; k++ {
		arr[k] = []any{float64(k + 1), float64(k + 1)}
	}
	arr[n] = []any{}
	raw, _ := json.Marshal(arr)
	done := make(chan error, 1)
	go func() {
		_, err := Decode(raw)
		done <- err
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("decode: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("DAG expansion did not complete (missing memoization)")
	}
}
