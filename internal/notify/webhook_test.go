package notify

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestSend_OK(t *testing.T) {
	var got payload
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("method: want POST, got %s", r.Method)
		}
		if ct := r.Header.Get("Content-Type"); ct != "application/json" {
			t.Errorf("Content-Type: want application/json, got %s", ct)
		}
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &got)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	n := New(srv.URL)
	if err := n.Send(context.Background(), "hello world"); err != nil {
		t.Fatal(err)
	}
	if got.Text != "hello world" {
		t.Errorf("payload text: want %q, got %q", "hello world", got.Text)
	}
}

func TestSend_HTTP4xx(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
	}))
	defer srv.Close()

	err := New(srv.URL).Send(context.Background(), "x")
	if err == nil {
		t.Fatal("want error for HTTP 403, got nil")
	}
}

func TestSend_HTTP5xx(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	err := New(srv.URL).Send(context.Background(), "x")
	if err == nil {
		t.Fatal("want error for HTTP 500, got nil")
	}
}

func TestSend_ContextCancelled(t *testing.T) {
	// Server that would succeed — but the context is already cancelled.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	err := New(srv.URL).Send(ctx, "x")
	if err == nil {
		t.Fatal("want error for cancelled context, got nil")
	}
}

func TestSend_Unreachable(t *testing.T) {
	// Use an address that is guaranteed to refuse connections.
	n := New("http://127.0.0.1:1")
	err := n.Send(context.Background(), "x")
	if err == nil {
		t.Fatal("want error for unreachable server, got nil")
	}
}
