package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"

	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

func integrationStore(t *testing.T) *chatStore {
	t.Helper()
	if os.Getenv("HARPER_STORAGE_TESTS") != "1" {
		t.Skip("set HARPER_STORAGE_TESTS=1 with a PostgreSQL connection to run storage integration tests")
	}
	db, err := gorm.Open(postgres.Open(""), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		t.Fatal("could not connect to integration database")
	}
	table := "harper_test_" + strings.ToLower(newID())
	images := db.Table(table + "_images")
	if err := images.AutoMigrate(&imageAttachment{}); err != nil {
		t.Fatal(err)
	}
	db = db.Table(table)
	if err := db.AutoMigrate(&conversation{}); err != nil {
		t.Fatal(err)
	}
	s := &chatStore{db: db, images: images}
	t.Cleanup(func() { _ = db.Migrator().DropTable(table + "_images"); _ = db.Migrator().DropTable(table); s.close() })
	return s
}

func TestGORMStorageTransactions(t *testing.T) {
	s := integrationStore(t)
	ctx := context.Background()
	input := chatRequest{ChatID: newID(), Model: "deepseek-flash", Messages: []message{{Role: "user", Content: "Keep this question"}}}
	c, err := s.begin(ctx, input)
	if err != nil {
		t.Fatal(err)
	}
	if len(c.Messages) != 2 || c.Messages[1].Status != "streaming" {
		t.Fatal("prompt and pending reply were not saved")
	}
	if _, err = s.begin(ctx, input); !errors.Is(err, errChatBusy) {
		t.Fatal("concurrent generation was not rejected")
	}
	answer := c.Messages[1]
	answer.Content, answer.Reasoning, answer.Status = "A durable answer", "Thinking text", "complete"
	if err := s.saveAnswer(ctx, c.ID, answer); err != nil {
		t.Fatal(err)
	}
	c, err = s.get(ctx, c.ID)
	if err != nil || c.Messages[1].Content != "A durable answer" || c.Messages[1].Reasoning != "Thinking text" {
		t.Fatal("reply did not round trip through JSONB storage")
	}
	input.Regenerate = true
	c, err = s.begin(ctx, input)
	if err != nil {
		t.Fatal(err)
	}
	if len(c.Messages) != 2 || c.Messages[0].Content != "Keep this question" || c.Messages[1].ID == answer.ID {
		t.Fatal("regeneration duplicated the user turn or reused the old reply")
	}
	if err := s.db.Delete(&conversation{}, &conversation{ID: c.ID}).Error; err != nil {
		t.Fatal(err)
	}
	if err := s.saveAnswer(ctx, c.ID, c.Messages[1]); err != nil {
		t.Fatal(err)
	}
	if _, err := s.get(ctx, c.ID); !errors.Is(err, gorm.ErrRecordNotFound) {
		t.Fatal("late stream write resurrected a deleted conversation")
	}
}

func TestPersistedHTTPConversationUsesDatabaseHistory(t *testing.T) {
	store := integrationStore(t)
	var histories [][]message
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var payload struct {
			Messages []message `json:"messages"`
		}
		_ = json.NewDecoder(r.Body).Decode(&payload)
		histories = append(histories, payload.Messages)
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"Saved reply\"}}]}\n\ndata: [DONE]\n\n")
	}))
	defer upstream.Close()
	s := &server{store: store, key: "fake-key", baseURL: upstream.URL, client: upstream.Client()}
	id := newID()
	for _, text := range []string{"First question", "Follow-up"} {
		body, _ := json.Marshal(chatRequest{ChatID: id, Model: "deepseek-flash", Messages: []message{{Role: "user", Content: text}}})
		w := httptest.NewRecorder()
		localSecurity(s.applicationRoutes()).ServeHTTP(w, httptest.NewRequest("POST", "/api/chat", bytes.NewReader(body)))
		if w.Code != 200 || !strings.Contains(w.Body.String(), "event: chat") || !strings.Contains(w.Body.String(), "data: [DONE]") {
			t.Fatalf("stream failed: %d %s", w.Code, w.Body.String())
		}
	}
	if len(histories[1]) != 3 || histories[1][0].Content != "First question" || histories[1][1].Content != "Saved reply" {
		t.Fatal("server did not use stored conversation context")
	}
	c, err := store.get(context.Background(), id)
	if err != nil || len(c.Messages) != 4 || c.Messages[3].Status != "complete" {
		t.Fatal("stream completion was not persisted before DONE")
	}
	w := httptest.NewRecorder()
	localSecurity(s.applicationRoutes()).ServeHTTP(w, httptest.NewRequest("GET", "/api/chats", nil))
	if !strings.Contains(w.Body.String(), "First question") {
		t.Fatal("history API did not expose saved chats")
	}
}

func TestBrowserHistoryImportIsIdempotent(t *testing.T) {
	store := integrationStore(t)
	s := &server{store: store}
	c := conversation{ID: newID(), Title: "Browser history", Model: "deepseek-flash", Messages: []savedMessage{
		{ID: newID(), Role: "user", Content: "Remember me"}, {ID: newID(), Role: "assistant", Content: "Partial", Status: "streaming"},
	}}
	for i := 0; i < 2; i++ {
		body, _ := json.Marshal(map[string]any{"chats": []conversation{c}})
		w := httptest.NewRecorder()
		localSecurity(s.applicationRoutes()).ServeHTTP(w, httptest.NewRequest("POST", "/api/chats/import", bytes.NewReader(body)))
		if w.Code != 200 || !strings.Contains(w.Body.String(), fmt.Sprintf(`"imported":%d`, 1-i)) {
			t.Fatalf("unexpected import: %s", w.Body.String())
		}
		c.Messages[0].Content = "A stale browser copy"
	}
	saved, err := store.get(context.Background(), c.ID)
	if err != nil || saved.Messages[0].Content != "Remember me" || saved.Messages[1].Status != "stopped" {
		t.Fatal("import overwrote existing history or failed to recover pending reply")
	}
	w := httptest.NewRecorder()
	localSecurity(s.applicationRoutes()).ServeHTTP(w, httptest.NewRequest("PATCH", "/api/chats/"+c.ID, strings.NewReader(`{"model":"deepseek-v4-pro","thinking":true}`)))
	if w.Code != 204 {
		t.Fatal("settings update failed")
	}
	saved, _ = store.get(context.Background(), c.ID)
	if saved.Model != "deepseek-v4-pro" || !saved.Thinking {
		t.Fatal("settings were not saved")
	}
	w = httptest.NewRecorder()
	localSecurity(s.applicationRoutes()).ServeHTTP(w, httptest.NewRequest("DELETE", "/api/chats/"+c.ID, nil))
	if w.Code != 204 {
		t.Fatal("deletion failed")
	}
}

func TestFailedUpstreamIsSaved(t *testing.T) {
	store := integrationStore(t)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(402); io.WriteString(w, "{}") }))
	defer upstream.Close()
	s := &server{store: store, key: "fake-key", baseURL: upstream.URL, client: upstream.Client()}
	id := newID()
	body, _ := json.Marshal(chatRequest{ChatID: id, Model: "deepseek-flash", Messages: []message{{Role: "user", Content: "Do not lose my prompt"}}})
	w := httptest.NewRecorder()
	localSecurity(s.applicationRoutes()).ServeHTTP(w, httptest.NewRequest("POST", "/api/chat", bytes.NewReader(body)))
	if w.Code != 402 {
		t.Fatal("upstream error was not forwarded")
	}
	c, err := store.get(context.Background(), id)
	if err != nil || c.Messages[0].Content != "Do not lose my prompt" || c.Messages[1].Status != "error" || c.Messages[1].Error == "" {
		t.Fatal("failed turn was not saved")
	}
}

func TestRedisModelCacheAndRefresh(t *testing.T) {
	if os.Getenv("HARPER_STORAGE_TESTS") != "1" {
		t.Skip("requires the integration Redis service")
	}
	cache := redis.NewClient(&redis.Options{Addr: os.Getenv("REDIS_ADDR"), MaxRetries: -1})
	defer cache.Close()
	if cache.Ping(context.Background()).Err() != nil {
		t.Fatal("Redis is unavailable")
	}
	calls := 0
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		io.WriteString(w, `{"data":[{"id":"deepseek-flash","name":"Flash"}]}`)
	}))
	defer upstream.Close()
	s := &server{key: newID(), cache: cache, baseURL: upstream.URL, client: upstream.Client()}
	defer cache.Del(context.Background(), s.modelCacheKey())
	for _, path := range []string{"/api/models", "/api/models", "/api/models?refresh=1"} {
		w := httptest.NewRecorder()
		localSecurity(s.applicationRoutes()).ServeHTTP(w, httptest.NewRequest("GET", path, nil))
		if w.Code != 200 {
			t.Fatal("model lookup failed")
		}
	}
	if calls != 2 {
		t.Fatalf("expected cache reuse and a forced refresh, got %d upstream calls", calls)
	}
	if ttl := cache.TTL(context.Background(), s.modelCacheKey()).Val(); ttl <= 0 || ttl > 5*time.Minute {
		t.Fatal("cache expiry was not set")
	}
	s.cache = redis.NewClient(&redis.Options{Addr: "127.0.0.1:1", DialTimeout: 10 * time.Millisecond, MaxRetries: -1})
	defer s.cache.Close()
	w := httptest.NewRecorder()
	localSecurity(s.applicationRoutes()).ServeHTTP(w, httptest.NewRequest("GET", "/api/models", nil))
	if w.Code != 200 {
		t.Fatal("Redis outage blocked model discovery")
	}
}

func TestCancelledReplyPreservesPartialText(t *testing.T) {
	store := integrationStore(t)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"Partial reply\"}}]}\n\n")
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	}))
	defer upstream.Close()
	s := &server{store: store, key: "fake-key", baseURL: upstream.URL, client: upstream.Client()}
	app := httptest.NewServer(localSecurity(s.applicationRoutes()))
	defer app.Close()
	id := newID()
	body, _ := json.Marshal(chatRequest{ChatID: id, Model: "deepseek-flash", Messages: []message{{Role: "user", Content: "Keep my partial reply"}}})
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "POST", app.URL+"/api/chat", bytes.NewReader(body))
	resp, err := app.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	reader := bufio.NewReader(resp.Body)
	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			t.Fatal("did not receive partial stream")
		}
		if strings.Contains(line, `"content":"Partial reply"`) {
			break
		}
	}
	cancel()
	resp.Body.Close()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		c, err := store.get(context.Background(), id)
		if err == nil && c.Messages[1].Status == "stopped" {
			if c.Messages[1].Content != "Partial reply" {
				t.Fatal("partial text was lost")
			}
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("cancellation was not persisted")
}
