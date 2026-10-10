package main

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"regexp"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"golang.org/x/crypto/bcrypt"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

const sessionCookie = "harper_session"
const sessionLifetime = 7 * 24 * time.Hour

// Each installation has exactly one owner. The fixed primary key makes first
// setup atomic even when multiple browsers submit at the same time.
type authAccount struct {
	ID           uint   `gorm:"primaryKey;autoIncrement:false"`
	Username     string `gorm:"not null;size:64"`
	PasswordHash string `gorm:"not null"`
	RecoveryHash string `gorm:"not null;size:64"`
}

type authSession struct {
	ID        string    `gorm:"primaryKey;size:64"`
	ExpiresAt time.Time `gorm:"not null;index"`
}

type authStore struct {
	db       *gorm.DB
	mu       sync.Mutex
	window   time.Time
	attempts int
}

var usernamePattern = regexp.MustCompile(`^[a-zA-Z0-9_.-]{1,64}$`)
var errAlreadySetup = errors.New("owner already configured")
var errCredentials = errors.New("invalid credentials")

func openAuth(ctx context.Context, db *gorm.DB) (*authStore, error) {
	if err := db.WithContext(ctx).AutoMigrate(&authAccount{}, &authSession{}); err != nil {
		return nil, err
	}
	return &authStore{db: db}, nil
}

func secretHash(secret string) string {
	hash := sha256.Sum256([]byte(secret))
	return hex.EncodeToString(hash[:])
}

// Bound expensive password checks for this single-owner installation. This
// doesn't trust client-supplied IP headers and also limits recovery-key guesses.
func (a *authStore) allowAttempt(now time.Time) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.window.IsZero() || now.Sub(a.window) >= time.Minute {
		a.window, a.attempts = now, 0
	}
	if a.attempts >= 10 {
		return false
	}
	a.attempts++
	return true
}

func (s *server) routes() http.Handler {
	app := s.applicationRoutes()
	mux := http.NewServeMux()
	mux.Handle("GET /api/health", app)
	mux.HandleFunc("GET /api/auth/status", s.authStatus)
	mux.HandleFunc("POST /api/auth/setup", s.authChange)
	mux.HandleFunc("POST /api/auth/login", s.authChange)
	mux.HandleFunc("POST /api/auth/reset", s.authChange)
	mux.HandleFunc("POST /api/auth/logout", s.authLogout)
	mux.Handle("/", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !s.authAvailable(w) {
			return
		}
		valid, err := s.auth.validSession(r.Context(), r)
		if err != nil {
			storageFailure(w)
			return
		}
		if !valid {
			writeAuthError(w, http.StatusUnauthorized, "Sign in to unlock your workspace.")
			return
		}
		app.ServeHTTP(w, r)
	}))
	return localSecurity(mux)
}

func writeAuthError(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, map[string]any{"error": map[string]string{"code": "UNAUTHENTICATED", "message": message}})
}

func (s *server) authAvailable(w http.ResponseWriter) bool {
	if s.auth == nil || s.auth.db == nil {
		writeError(w, http.StatusServiceUnavailable, "Account storage is unavailable. Please retry.")
		return false
	}
	return true
}

func (a *authStore) validSession(ctx context.Context, r *http.Request) (bool, error) {
	cookie, err := r.Cookie(sessionCookie)
	if err != nil || len(cookie.Value) != 64 {
		return false, nil
	}
	var session authSession
	err = a.db.WithContext(ctx).Where("id = ? AND expires_at > ?", secretHash(cookie.Value), time.Now()).First(&session).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return false, nil
	}
	return err == nil, err
}

func (s *server) authStatus(w http.ResponseWriter, r *http.Request) {
	if !s.authAvailable(w) {
		return
	}
	var account authAccount
	err := s.auth.db.WithContext(r.Context()).First(&account, 1).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		writeJSON(w, http.StatusOK, map[string]any{"setupRequired": true, "authenticated": false})
		return
	}
	if err != nil {
		storageFailure(w)
		return
	}
	valid, err := s.auth.validSession(r.Context(), r)
	if err != nil {
		storageFailure(w)
		return
	}
	result := map[string]any{"setupRequired": false, "authenticated": valid}
	if valid {
		result["username"] = account.Username
	}
	writeJSON(w, http.StatusOK, result)
}

type authInput struct {
	Username    string `json:"username"`
	Password    string `json:"password"`
	RecoveryKey string `json:"recoveryKey,omitempty"`
}

func (s *server) authChange(w http.ResponseWriter, r *http.Request) {
	if !s.authAvailable(w) {
		return
	}
	if !s.auth.allowAttempt(time.Now()) {
		w.Header().Set("Retry-After", "60")
		writeError(w, http.StatusTooManyRequests, "Too many attempts. Wait one minute and try again.")
		return
	}
	if !strings.HasPrefix(r.Header.Get("Content-Type"), "application/json") {
		writeError(w, http.StatusUnsupportedMediaType, "Send account details as JSON.")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 8<<10)
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	var input authInput
	if err := decoder.Decode(&input); err != nil || decoder.Decode(&struct{}{}) != io.EOF {
		writeError(w, http.StatusBadRequest, "Invalid account details.")
		return
	}
	input.Username = strings.TrimSpace(input.Username)
	login := r.URL.Path == "/api/auth/login"
	setup := r.URL.Path == "/api/auth/setup"
	if !usernamePattern.MatchString(input.Username) || len(input.Password) > 72 || len(input.Password) == 0 {
		writeError(w, http.StatusBadRequest, "Use a username of 1–64 letters, numbers, dots, underscores or hyphens, and a password of at most 72 bytes.")
		return
	}
	if !login && utf8.RuneCountInString(input.Password) < 12 {
		writeError(w, http.StatusBadRequest, "Choose a master password of at least 12 characters (up to 72 bytes).")
		return
	}
	var passwordHash []byte
	var recoveryKey string
	if !login {
		var err error
		passwordHash, err = bcrypt.GenerateFromPassword([]byte(input.Password), bcrypt.DefaultCost)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "Could not secure the account. Please retry.")
			return
		}
		recoveryKey = "HARPER-" + rand.Text()
	}
	var tokenBytes [32]byte
	_, _ = rand.Read(tokenBytes[:])
	token := hex.EncodeToString(tokenBytes[:])
	expires := time.Now().Add(sessionLifetime)
	err := s.auth.db.WithContext(r.Context()).Transaction(func(tx *gorm.DB) error {
		if setup {
			account := authAccount{ID: 1, Username: input.Username, PasswordHash: string(passwordHash), RecoveryHash: secretHash(recoveryKey)}
			result := tx.Clauses(clause.OnConflict{DoNothing: true}).Create(&account)
			if result.Error != nil {
				return result.Error
			}
			if result.RowsAffected != 1 {
				return errAlreadySetup
			}
		} else {
			var account authAccount
			if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).First(&account, 1).Error; err != nil {
				if errors.Is(err, gorm.ErrRecordNotFound) {
					return errCredentials
				}
				return err
			}
			if login {
				// Compare the password even for an incorrect username.
				passwordErr := bcrypt.CompareHashAndPassword([]byte(account.PasswordHash), []byte(input.Password))
				if passwordErr != nil || input.Username != account.Username {
					return errCredentials
				}
			} else {
				if subtle.ConstantTimeCompare([]byte(secretHash(strings.TrimSpace(input.RecoveryKey))), []byte(account.RecoveryHash)) != 1 {
					return errCredentials
				}
				account.Username, account.PasswordHash, account.RecoveryHash = input.Username, string(passwordHash), secretHash(recoveryKey)
				if err := tx.Save(&account).Error; err != nil {
					return err
				}
				// Reset preserves chat history, but revokes every old login and
				// rotates the recovery key in this same transaction.
				if err := tx.Where("1 = 1").Delete(&authSession{}).Error; err != nil {
					return err
				}
			}
		}
		if err := tx.Where("expires_at <= ?", time.Now()).Delete(&authSession{}).Error; err != nil {
			return err
		}
		return tx.Create(&authSession{ID: secretHash(token), ExpiresAt: expires}).Error
	})
	if errors.Is(err, errAlreadySetup) {
		writeError(w, http.StatusConflict, "This installation already has an owner. Sign in or reset your account.")
		return
	}
	if errors.Is(err, errCredentials) {
		message := "Incorrect username or master password."
		if !login {
			message = "The recovery key is incorrect."
		}
		writeAuthError(w, http.StatusUnauthorized, message)
		return
	}
	if err != nil {
		storageFailure(w)
		return
	}
	setSessionCookie(w, r, token, expires, int(sessionLifetime.Seconds()))
	result := map[string]any{"authenticated": true, "username": input.Username}
	if recoveryKey != "" {
		result["recoveryKey"] = recoveryKey
	}
	writeJSON(w, http.StatusOK, result)
}

func setSessionCookie(w http.ResponseWriter, r *http.Request, value string, expires time.Time, maxAge int) {
	http.SetCookie(w, &http.Cookie{
		Name: sessionCookie, Value: value, Path: "/", Expires: expires, MaxAge: maxAge,
		HttpOnly: true, SameSite: http.SameSiteStrictMode,
		Secure: r.TLS != nil || os.Getenv("AUTH_COOKIE_SECURE") == "true",
	})
}

func (s *server) authLogout(w http.ResponseWriter, r *http.Request) {
	if !s.authAvailable(w) {
		return
	}
	if cookie, err := r.Cookie(sessionCookie); err == nil {
		if err := s.auth.db.WithContext(r.Context()).Where("id = ?", secretHash(cookie.Value)).Delete(&authSession{}).Error; err != nil {
			storageFailure(w)
			return
		}
	}
	setSessionCookie(w, r, "", time.Unix(1, 0), -1)
	w.WriteHeader(http.StatusNoContent)
}
