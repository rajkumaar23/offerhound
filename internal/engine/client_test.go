package engine

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestClientStripsTokenOnCrossHostRedirect(t *testing.T) {
	var sawTokenAtB string
	b := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sawTokenAtB = r.Header.Get("X-Offerhound-Token")
		w.Write([]byte("{}"))
	}))
	defer b.Close()
	a := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, b.URL+"/landed", http.StatusFound)
	}))
	defer a.Close()

	c := NewClient(a.URL, "super-secret")
	if err := c.do(context.Background(), "GET", "/x", nil, nil); err != nil {
		t.Fatal(err)
	}
	if sawTokenAtB != "" {
		t.Fatalf("write token leaked to redirect target host: %q", sawTokenAtB)
	}
}
