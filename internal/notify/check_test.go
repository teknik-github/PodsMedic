package notify

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestSlackCheck(t *testing.T) {
	cases := []struct {
		name string
		url  string
		ok   bool
	}{
		{"valid", "https://hooks.slack.com/services/T01/B02/abcdef", true},
		{"placeholder", "https://hooks.slack.com/services/T000/B000/xxxx", false},
		{"http not https", "http://hooks.slack.com/services/T01/B02/x", false},
		{"wrong host", "https://evil.example.com/services/T01/B02/x", false},
		{"wrong path", "https://hooks.slack.com/webhook/x", false},
		{"garbage", "://nope", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := NewSlack(c.url).Check(context.Background())
			if c.ok && err != nil {
				t.Fatalf("expected ok, got %v", err)
			}
			if !c.ok && err == nil {
				t.Fatal("expected an error, got nil")
			}
		})
	}
}

func TestTelegramCheckOK(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte(`{"ok":true,"result":{}}`))
	}))
	defer srv.Close()

	tg := NewTelegram("token", "123")
	tg.baseURL = srv.URL
	if err := tg.Check(context.Background()); err != nil {
		t.Fatalf("expected a healthy check, got %v", err)
	}
}

func TestTelegramCheckBadToken(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		w.Write([]byte(`{"ok":false,"error_code":401,"description":"Unauthorized"}`))
	}))
	defer srv.Close()

	tg := NewTelegram("bad", "123")
	tg.baseURL = srv.URL
	err := tg.Check(context.Background())
	if err == nil {
		t.Fatal("expected an error for a bad token")
	}
	if !strings.Contains(err.Error(), "Unauthorized") {
		t.Fatalf("error should surface the API description, got %v", err)
	}
}

func TestStdoutCheckAlwaysOK(t *testing.T) {
	if err := NewStdout(&strings.Builder{}).Check(context.Background()); err != nil {
		t.Fatalf("stdout check should never fail, got %v", err)
	}
}

func TestMultiCheckEach(t *testing.T) {
	m := Multi{
		NewStdout(&strings.Builder{}),
		NewSlack("https://hooks.slack.com/services/T000/B000/xxxx"), // placeholder → fails
	}
	results := m.CheckEach(context.Background())
	if len(results) != 2 {
		t.Fatalf("want 2 results, got %d", len(results))
	}
	var failures int
	for _, r := range results {
		if r.Err != nil {
			failures++
		}
	}
	if failures != 1 {
		t.Fatalf("expected exactly the placeholder slack to fail, got %d failures", failures)
	}
}
