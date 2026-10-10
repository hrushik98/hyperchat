package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestChatStreamsAndKeepsKeyServerSide(t *testing.T) {
	const stream = "data: {\"choices\":[{\"delta\":{\"content\":\"Hello\"}}]}\n\ndata: [DONE]\n\n"
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/chat/completions" || r.Header.Get("Authorization") != "Bearer test-secret" {
			t.Error("missing endpoint or server-side authorization")
		}
		var payload map[string]any
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Error(err)
		}
		if payload["stream"] != true || payload["model"] != "deepseek-flash" || payload["thinking"].(map[string]any)["type"] != "enabled" {
			t.Error("chat options did not reach DeepSeek")
		}
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, stream)
	}))
	defer upstream.Close()
	s := &server{key: "test-secret", baseURL: upstream.URL, client: upstream.Client()}
	req := httptest.NewRequest("POST", "/api/chat", strings.NewReader(`{"model":"deepseek-flash","thinking":true,"messages":[{"role":"user","content":"Hi"}]}`))
	w := httptest.NewRecorder()
	localSecurity(s.applicationRoutes()).ServeHTTP(w, req)
	if w.Code != 200 || w.Body.String() != stream || strings.Contains(w.Body.String(), s.key) {
		t.Fatalf("unexpected response: %d %s", w.Code, w.Body.String())
	}
	if w.Header().Get("X-Accel-Buffering") != "no" {
		t.Error("proxy buffering must be disabled")
	}
}

func TestInvalidRequestsNeverReachDeepSeek(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("invalid request reached upstream")
	}))
	defer upstream.Close()
	s := &server{key: "secret", baseURL: upstream.URL, client: upstream.Client()}
	cases := []string{
		`{`,
		`{"model":"deepseek-flash","messages":[]}`,
		`{"model":"bad model","messages":[{"role":"user","content":"Hi"}]}`,
		`{"model":"deepseek-flash","messages":[{"role":"system","content":"Hi"}]}`,
		`{"model":"deepseek-flash","messages":[{"role":"user","content":" "}]}`,
		`{"model":"deepseek-flash","messages":[{"role":"assistant","content":"Hi"}]}`,
		`{"model":"deepseek-flash","messages":[{"role":"user","content":"Hi"}],"api_key":"untrusted"}`,
		`{"model":"deepseek-flash","messages":[{"role":"user","content":"Hi"}]} {}`,
	}
	for _, body := range cases {
		t.Run(body, func(t *testing.T) {
			w := httptest.NewRecorder()
			localSecurity(s.applicationRoutes()).ServeHTTP(w, httptest.NewRequest("POST", "/api/chat", strings.NewReader(body)))
			if w.Code != http.StatusBadRequest {
				t.Fatalf("got %d, expected 400", w.Code)
			}
		})
	}
}

func TestAPIKeyAndBalanceErrors(t *testing.T) {
	for status, expected := range map[int]string{401: "rejected the API key", 402: "balance is insufficient", 429: "too many requests", 503: "temporarily unavailable", 400: "[redacted]"} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(status)
				io.WriteString(w, `{"error":{"message":"invalid secret-key"}}`)
			}))
			defer upstream.Close()
			s := &server{key: "secret-key", baseURL: upstream.URL, client: upstream.Client()}
			w := httptest.NewRecorder()
			localSecurity(s.applicationRoutes()).ServeHTTP(w, httptest.NewRequest("GET", "/api/models", nil))
			if w.Code != status || !strings.Contains(w.Body.String(), expected) || strings.Contains(w.Body.String(), s.key) {
				t.Fatalf("unexpected error response: %d %s", w.Code, w.Body.String())
			}
		})
	}
}

func TestModelDiscoveryAndCrossSiteProtection(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"data":[{"id":"new-model","name":"New Model","internal":"ignore"}]}`)
	}))
	defer upstream.Close()
	s := &server{key: "secret", baseURL: upstream.URL, client: upstream.Client()}
	w := httptest.NewRecorder()
	localSecurity(s.applicationRoutes()).ServeHTTP(w, httptest.NewRequest("GET", "/api/models", nil))
	if w.Code != 200 || !strings.Contains(w.Body.String(), "new-model") || strings.Contains(w.Body.String(), "internal") {
		t.Error("model discovery failed")
	}
	req := httptest.NewRequest("GET", "/api/models", nil)
	req.Header.Set("Origin", "https://another-site.example")
	w = httptest.NewRecorder()
	localSecurity(s.applicationRoutes()).ServeHTTP(w, req)
	if w.Code != http.StatusForbidden {
		t.Error("cross-site browser requests must be rejected")
	}
}

func TestStoppingChatCancelsUpstream(t *testing.T) {
	started, cancelled := make(chan struct{}), make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
		close(started)
		<-r.Context().Done()
		close(cancelled)
	}))
	defer upstream.Close()
	s := &server{key: "secret", baseURL: upstream.URL, client: upstream.Client()}
	app := httptest.NewServer(localSecurity(s.applicationRoutes()))
	defer app.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "POST", app.URL+"/api/chat", strings.NewReader(`{"model":"deepseek-flash","messages":[{"role":"user","content":"Hi"}]}`))
	resp, err := app.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	<-started
	cancel()
	select {
	case <-cancelled:
	case <-time.After(2 * time.Second):
		t.Fatal("stopping a response did not cancel DeepSeek request")
	}
}
