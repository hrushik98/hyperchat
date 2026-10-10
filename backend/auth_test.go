package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/crypto/bcrypt"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
	"gorm.io/gorm/schema"
)

func authRequest(t *testing.T, handler http.Handler, method, path string, input any, cookie *http.Cookie) *httptest.ResponseRecorder {
	t.Helper()
	var body strings.Reader
	if input != nil {
		data, err := json.Marshal(input)
		if err != nil {
			t.Fatal(err)
		}
		body = *strings.NewReader(string(data))
	}
	r := httptest.NewRequest(method, path, &body)
	if input != nil {
		r.Header.Set("Content-Type", "application/json")
	}
	if cookie != nil {
		r.AddCookie(cookie)
	}
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, r)
	return w
}

func authResult(t *testing.T, w *httptest.ResponseRecorder) (map[string]any, *http.Cookie) {
	t.Helper()
	if w.Code != http.StatusOK {
		t.Fatalf("account operation failed: %d %s", w.Code, w.Body.String())
	}
	var result map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	var cookie *http.Cookie
	for _, c := range w.Result().Cookies() {
		if c.Name == sessionCookie {
			cookie = c
		}
	}
	return result, cookie
}

func integrationAuth(t *testing.T) *authStore {
	t.Helper()
	if os.Getenv("HARPER_STORAGE_TESTS") != "1" {
		t.Skip("set HARPER_STORAGE_TESTS=1 with PostgreSQL to run account integration tests")
	}
	db, err := gorm.Open(postgres.Open(""), &gorm.Config{
		Logger:         logger.Default.LogMode(logger.Silent),
		NamingStrategy: schema.NamingStrategy{TablePrefix: "harper_auth_test_" + strings.ToLower(newID()) + "_"},
	})
	if err != nil {
		t.Fatal(err)
	}
	a, err := openAuth(context.Background(), db)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.AutoMigrate(&conversation{}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = db.Migrator().DropTable(&authSession{}, &authAccount{}, &conversation{})
		sqlDB, _ := db.DB()
		_ = sqlDB.Close()
	})
	return a
}

func TestAccountLifecycleAndProtectedRoutes(t *testing.T) {
	a := integrationAuth(t)
	upstreamCalls := 0
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamCalls++
		writeJSON(w, 200, map[string]any{"data": []any{map[string]string{"id": "deepseek-flash"}}})
	}))
	defer upstream.Close()
	s := &server{auth: a, store: &chatStore{db: a.db}, key: "fake", baseURL: upstream.URL, client: upstream.Client()}
	handler := s.routes()
	status, _ := authResult(t, authRequest(t, handler, "GET", "/api/auth/status", nil, nil))
	if status["setupRequired"] != true || status["authenticated"] != false {
		t.Fatal("new installation did not require setup")
	}
	for _, route := range []struct{ method, path string }{
		{"GET", "/api/models"}, {"GET", "/api/chats"}, {"GET", "/api/chats/example"},
		{"GET", "/api/images/example"}, {"POST", "/api/chat"}, {"POST", "/api/chats/import"},
		{"PATCH", "/api/chats/example"}, {"DELETE", "/api/chats/example"},
	} {
		w := authRequest(t, handler, route.method, route.path, nil, nil)
		if w.Code != 401 || !strings.Contains(w.Body.String(), "UNAUTHENTICATED") {
			t.Fatalf("unprotected route: %s %s: %d", route.method, route.path, w.Code)
		}
	}
	if upstreamCalls != 0 {
		t.Fatal("logged-out browser could spend API credits")
	}
	owner := authInput{Username: "owner", Password: "correct horse battery"}
	setup, firstCookie := authResult(t, authRequest(t, handler, "POST", "/api/auth/setup", owner, nil))
	key, ok := setup["recoveryKey"].(string)
	if !ok || key == "" || firstCookie == nil || !firstCookie.HttpOnly || firstCookie.SameSite != http.SameSiteStrictMode || firstCookie.MaxAge != int(sessionLifetime.Seconds()) {
		t.Fatal("setup did not provide recovery and a protected session cookie")
	}
	if w := authRequest(t, handler, "POST", "/api/auth/setup", owner, nil); w.Code != 409 {
		t.Fatal("a second owner was allowed")
	}
	var stored authAccount
	if err := a.db.First(&stored, 1).Error; err != nil {
		t.Fatal(err)
	}
	if stored.PasswordHash == owner.Password || stored.RecoveryHash == key || bcrypt.CompareHashAndPassword([]byte(stored.PasswordHash), []byte(owner.Password)) != nil {
		t.Fatal("account secrets were not hashed correctly")
	}
	var session authSession
	if err := a.db.First(&session).Error; err != nil || session.ID == firstCookie.Value {
		t.Fatal("session token was not hashed in storage")
	}
	for _, invalid := range []authInput{{Username: "owner", Password: "incorrect"}, {Username: "other", Password: owner.Password}} {
		if w := authRequest(t, handler, "POST", "/api/auth/login", invalid, nil); w.Code != 401 {
			t.Fatal("invalid login succeeded")
		}
	}
	login, secondCookie := authResult(t, authRequest(t, handler, "POST", "/api/auth/login", owner, nil))
	if login["recoveryKey"] != nil || secondCookie.Value == firstCookie.Value {
		t.Fatal("login exposed recovery or reused a token")
	}
	// Reopening storage models a backend restart without discarding sessions.
	reopened, err := openAuth(context.Background(), a.db)
	if err != nil {
		t.Fatal(err)
	}
	s.auth = reopened
	handler = s.routes()
	status, _ = authResult(t, authRequest(t, handler, "GET", "/api/auth/status", nil, firstCookie))
	if status["authenticated"] != true || status["username"] != "owner" {
		t.Fatal("session did not survive reopening storage")
	}
	if w := authRequest(t, handler, "GET", "/api/models", nil, firstCookie); w.Code != 200 || upstreamCalls != 1 {
		t.Fatal("authenticated model access failed")
	}
	chat := conversation{ID: newID(), Title: "Keep my history", Model: "deepseek-flash", UpdatedAt: time.Now().UnixMilli(), Messages: []savedMessage{{ID: newID(), Role: "user", Content: "Private question"}}}
	if err := a.db.Create(&chat).Error; err != nil {
		t.Fatal(err)
	}
	reset := authInput{Username: "new-owner", Password: "a different passphrase", RecoveryKey: "wrong-key"}
	if w := authRequest(t, handler, "POST", "/api/auth/reset", reset, nil); w.Code != 401 {
		t.Fatal("reset without valid recovery succeeded")
	}
	status, _ = authResult(t, authRequest(t, handler, "GET", "/api/auth/status", nil, firstCookie))
	if status["authenticated"] != true {
		t.Fatal("failed reset invalidated a legitimate session")
	}
	reset.RecoveryKey = key
	resetResult, resetCookie := authResult(t, authRequest(t, handler, "POST", "/api/auth/reset", reset, nil))
	if resetResult["recoveryKey"] == key || resetCookie == nil {
		t.Fatal("reset did not rotate recovery and session")
	}
	for _, cookie := range []*http.Cookie{firstCookie, secondCookie} {
		if w := authRequest(t, handler, "GET", "/api/chats", nil, cookie); w.Code != 401 {
			t.Fatal("reset did not revoke all old sessions")
		}
	}
	if w := authRequest(t, handler, "GET", "/api/chats", nil, resetCookie); w.Code != 200 || !strings.Contains(w.Body.String(), "Private question") {
		t.Fatal("reset lost chat history")
	}
	if w := authRequest(t, handler, "POST", "/api/auth/reset", reset, nil); w.Code != 401 {
		t.Fatal("old recovery key remained usable")
	}
	if w := authRequest(t, handler, "POST", "/api/auth/login", owner, nil); w.Code != 401 {
		t.Fatal("old credentials remained usable")
	}
	reset.RecoveryKey = ""
	_, newCookie := authResult(t, authRequest(t, handler, "POST", "/api/auth/login", reset, nil))
	if w := authRequest(t, handler, "POST", "/api/auth/logout", nil, newCookie); w.Code != 204 || w.Result().Cookies()[0].MaxAge != -1 {
		t.Fatal("logout did not clear the browser cookie")
	}
	if w := authRequest(t, handler, "GET", "/api/chats", nil, newCookie); w.Code != 401 {
		t.Fatal("logged-out session still had access")
	}
	if err := a.db.Model(&authSession{}).Where("id = ?", secretHash(resetCookie.Value)).Update("expires_at", time.Now().Add(-time.Second)).Error; err != nil {
		t.Fatal(err)
	}
	if w := authRequest(t, handler, "GET", "/api/chats", nil, resetCookie); w.Code != 401 {
		t.Fatal("expired session still had access")
	}
}

func TestConcurrentFirstSetup(t *testing.T) {
	a := integrationAuth(t)
	handler := (&server{auth: a}).routes()
	var group sync.WaitGroup
	results := make(chan int, 2)
	for range 2 {
		group.Go(func() {
			w := authRequest(t, handler, "POST", "/api/auth/setup", authInput{Username: "owner", Password: "correct horse battery"}, nil)
			results <- w.Code
		})
	}
	group.Wait()
	close(results)
	counts := map[int]int{}
	for status := range results {
		counts[status]++
	}
	if counts[200] != 1 || counts[409] != 1 {
		t.Fatalf("setup was not atomic: %v", counts)
	}
}

func TestAuthFailsClosedAndRejectsCrossSiteRequests(t *testing.T) {
	handler := (&server{}).routes()
	for _, path := range []string{"/api/chat", "/api/models", "/api/auth/status"} {
		if w := authRequest(t, handler, "GET", path, nil, nil); w.Code != 503 {
			t.Fatalf("missing account storage allowed access: %s %d", path, w.Code)
		}
	}
	for _, path := range []string{"/api/auth/setup", "/api/auth/login", "/api/auth/reset", "/api/auth/logout"} {
		r := httptest.NewRequest("POST", path, nil)
		r.Header.Set("Origin", "https://another-site.example")
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		if w.Code != 403 {
			t.Fatal("cross-site account request was allowed")
		}
	}
}

func TestAuthRateLimitAndSecureCookies(t *testing.T) {
	a := &authStore{}
	now := time.Now()
	for range 10 {
		if !a.allowAttempt(now) {
			t.Fatal("allowed attempts rejected too early")
		}
	}
	if a.allowAttempt(now) || !a.allowAttempt(now.Add(time.Minute)) {
		t.Fatal("account attempt limit or expiry failed")
	}
	t.Setenv("AUTH_COOKIE_SECURE", "true")
	w := httptest.NewRecorder()
	setSessionCookie(w, httptest.NewRequest("GET", "/", nil), "token", now.Add(sessionLifetime), int(sessionLifetime.Seconds()))
	if !w.Result().Cookies()[0].Secure {
		t.Fatal("HTTPS deployment cookie did not use Secure")
	}
}

func TestAccountInputValidationAndRateLimitedResponse(t *testing.T) {
	a := integrationAuth(t)
	handler := (&server{auth: a}).routes()
	for _, input := range []authInput{
		{Username: "owner", Password: "short"}, {Username: "bad name", Password: "correct horse battery"},
		{Username: "owner", Password: strings.Repeat("a", 73)}, {Username: "owner", Password: strings.Repeat("界", 5)},
	} {
		if w := authRequest(t, handler, "POST", "/api/auth/setup", input, nil); w.Code != 400 {
			t.Fatal("invalid account details were accepted")
		}
	}
	for _, body := range []string{`{`, `{"username":"owner","password":"correct horse battery","extra":true}`, `{"username":"owner","password":"correct horse battery"} {}`, strings.Repeat("a", 9000)} {
		r := httptest.NewRequest("POST", "/api/auth/setup", strings.NewReader(body))
		r.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		if w.Code != 400 {
			t.Fatal("invalid request body was accepted")
		}
	}
	for range 2 {
		authRequest(t, handler, "POST", "/api/auth/login", authInput{Username: "owner", Password: "wrong"}, nil)
	}
	w := authRequest(t, handler, "POST", "/api/auth/login", authInput{Username: "owner", Password: "wrong"}, nil)
	if w.Code != 429 || w.Header().Get("Retry-After") != "60" {
		t.Fatal("account endpoint did not enforce attempt limit")
	}
}
