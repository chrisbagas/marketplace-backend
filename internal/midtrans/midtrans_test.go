package midtrans

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestCheckCredentials(t *testing.T) {
	cases := map[string]struct {
		status int
		body   string
		want   func(error) bool
	}{
		"kunci benar (404 transaksi tidak ada)": {200, `{"status_code":"404","status_message":"Transaction doesn't exist."}`, func(e error) bool { return e == nil }},
		"kunci ditolak (401)":                   {401, `{"status_message":"Access denied"}`, func(e error) bool { var a *APIError; return errors.As(e, &a) && a.Status == 401 }},
	}
	for name, tc := range cases {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(tc.status)
			w.Write([]byte(tc.body))
		}))
		c := New("k", "c", false)
		c.APIURL, c.SnapURL = srv.URL, srv.URL
		if err := c.CheckCredentials(context.Background()); !tc.want(err) {
			t.Errorf("%s: err = %v", name, err)
		}
		srv.Close()
	}
	// server tak terjangkau → galat jaringan, bukan APIError
	c := New("k", "c", false)
	c.APIURL = "http://127.0.0.1:1"
	var a *APIError
	if err := c.CheckCredentials(context.Background()); err == nil || errors.As(err, &a) {
		t.Errorf("galat jaringan: %v", err)
	}
}
