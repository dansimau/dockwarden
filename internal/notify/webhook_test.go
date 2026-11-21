package notify

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestWebhookJSON(t *testing.T) {
	var got Report
	var hdr string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hdr = r.Header.Get("X-Token")
		json.NewDecoder(r.Body).Decode(&got)
	}))
	defer srv.Close()
	w, err := New(Options{URL: srv.URL, Headers: map[string]string{"X-Token": "t"}})
	if err != nil {
		t.Fatal(err)
	}
	r := &Report{Updated: []Item{{Container: "web", Image: "nginx:1"}}}
	if err := w.Send(context.Background(), r); err != nil {
		t.Fatal(err)
	}
	if got.Updated[0].Container != "web" || hdr != "t" {
		t.Fatalf("got %+v hdr=%q", got, hdr)
	}
}

func TestWebhookPolicy(t *testing.T) {
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls++ }))
	defer srv.Close()
	w, _ := New(Options{URL: srv.URL})
	w.Send(context.Background(), &Report{})
	if calls != 0 {
		t.Fatal("should not send without changes")
	}
	w.On = "errors"
	w.Send(context.Background(), &Report{Updated: []Item{{}}})
	if calls != 0 {
		t.Fatal("errors policy should not send on updates")
	}
	w.Send(context.Background(), &Report{Errors: []string{"x"}})
	w.On = "always"
	w.Send(context.Background(), &Report{})
	if calls != 2 {
		t.Fatalf("calls=%d", calls)
	}
	var nilHook *Webhook
	if err := nilHook.Send(context.Background(), &Report{}); err != nil {
		t.Fatal("nil webhook must be a no-op")
	}
}

func TestWebhookTemplate(t *testing.T) {
	var body string
	var ct string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		body = string(b)
		ct = r.Header.Get("Content-Type")
	}))
	defer srv.Close()
	w, err := New(Options{URL: srv.URL, Template: `{"text": {{ .Summary | json }} }`, Method: "PUT"})
	if err != nil {
		t.Fatal(err)
	}
	if err := w.Send(context.Background(), &Report{Summary: "1 updated", Updated: []Item{{}}}); err != nil {
		t.Fatal(err)
	}
	if body != `{"text": "1 updated" }` || ct != "application/json" {
		t.Fatalf("body=%q ct=%q", body, ct)
	}
}

func TestWebhookError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(500) }))
	defer srv.Close()
	w, _ := New(Options{URL: srv.URL, On: "always"})
	if err := w.Send(context.Background(), &Report{}); err == nil {
		t.Fatal("expected error")
	}
	if n, err := New(Options{}); n != nil || err != nil {
		t.Fatal("no URL should yield nil")
	}
	if _, err := New(Options{URL: "x", Template: "{{"}); err == nil {
		t.Fatal("bad template")
	}
}
