package devalue

import (
	"encoding/json"
	"reflect"
	"testing"
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
