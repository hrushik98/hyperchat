package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"mime/multipart"
	"net/http"
	"os"
	"os/signal"
	"regexp"
	"strings"
	"syscall"
	"time"

	"github.com/redis/go-redis/v9"
	"gorm.io/gorm"
)

type server struct {
	key     string
	baseURL string
	client  *http.Client
	store   *chatStore
	cache   *redis.Client
	objects *objectStore
	auth    *authStore
}

type message struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type chatRequest struct {
	Model         string    `json:"model"`
	Messages      []message `json:"messages"`
	Thinking      bool      `json:"thinking"`
	ChatID        string    `json:"chatId"`
	Regenerate    bool      `json:"regenerate"`
	AttachmentIDs []string  `json:"attachmentIds,omitempty"`
}

var modelID = regexp.MustCompile(`^[a-zA-Z0-9._:-]{1,100}$`)

func main() {
	if len(os.Args) > 1 && os.Args[1] == "-healthcheck" {
		client := &http.Client{Timeout: 3 * time.Second}
		resp, err := client.Get("http://127.0.0.1:4004/api/health")
		if err != nil {
			os.Exit(1)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			os.Exit(1)
		}
		return
	}
	key := strings.TrimSpace(os.Getenv("DEEPSEEK_API_KEY"))
	if key == "" {
		log.Fatal("DEEPSEEK_API_KEY is missing. Add it to the project .env file.")
	}
	baseURL := strings.TrimRight(os.Getenv("DEEPSEEK_BASE_URL"), "/")
	if baseURL == "" {
		baseURL = "https://api.deepseek.com"
	}
	s := &server{key: key, baseURL: baseURL, client: &http.Client{Timeout: 10 * time.Minute}}
	startupCtx, startupCancel := context.WithTimeout(context.Background(), 30*time.Second)
	store, err := openStore(startupCtx)
	startupCancel()
	if err != nil {
		log.Fatal("Could not initialize PostgreSQL. Check the database service and PG* settings.")
	}
	s.store = store
	defer store.close()
	authCtx, authCancel := context.WithTimeout(context.Background(), 15*time.Second)
	s.auth, err = openAuth(authCtx, store.db)
	authCancel()
	if err != nil {
		log.Fatal("Could not initialize the owner account storage.")
	}
	objectCtx, objectCancel := context.WithTimeout(context.Background(), 30*time.Second)
	s.objects, err = openObjects(objectCtx)
	objectCancel()
	if err != nil {
		log.Fatal("Could not initialize object storage. Check the object-storage service and S3 settings.")
	}
	redisAddr := os.Getenv("REDIS_ADDR")
	if redisAddr == "" {
		redisAddr = "127.0.0.1:6379"
	}
	s.cache = redis.NewClient(&redis.Options{Addr: redisAddr, DialTimeout: time.Second, ReadTimeout: time.Second, WriteTimeout: time.Second, MaxRetries: -1})
	defer s.cache.Close()
	httpServer := &http.Server{
		Addr: ":4004", Handler: s.routes(), ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout: 30 * time.Second, IdleTimeout: 60 * time.Second,
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go s.cleanImages(ctx)
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = httpServer.Shutdown(shutdownCtx)
	}()
	log.Println("Harper backend listening on :4004")
	if err := httpServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Fatal(err)
	}
}

// applicationRoutes contains chat handlers; routes adds authentication to every
// application endpoint before exposing them to the network.
func (s *server) applicationRoutes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/health", func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
		defer cancel()
		if s.store != nil && s.store.ping(ctx) != nil {
			storageFailure(w)
			return
		}
		cacheStatus := "unavailable"
		if s.cache != nil && s.cache.Ping(ctx).Err() == nil {
			cacheStatus = "connected"
		}
		objectStatus := "unavailable"
		if s.objects != nil {
			if exists, err := s.objects.client.BucketExists(ctx, s.objects.bucket); err == nil && exists {
				objectStatus = "connected"
			}
		}
		writeJSON(w, http.StatusOK, map[string]any{"objectStorage": objectStatus, "status": "ok", "configured": s.key != "", "postgres": "connected", "redis": cacheStatus})
	})
	if s.store != nil {
		mux.HandleFunc("GET /api/images/{id}", s.serveImage)
		mux.HandleFunc("GET /api/chats", s.listChats)
		mux.HandleFunc("GET /api/chats/{id}", s.getChat)
		mux.HandleFunc("PATCH /api/chats/{id}", s.updateChat)
		mux.HandleFunc("DELETE /api/chats/{id}", s.deleteChat)
		mux.HandleFunc("POST /api/chats/import", s.importChats)
	}
	mux.HandleFunc("GET /api/models", s.models)
	mux.HandleFunc("POST /api/chat", s.chat)
	return mux
}

func localSecurity(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Cache-Control", "no-store")
		// The frontend uses a same-origin proxy. Reject cross-site browser access
		// so other websites cannot spend the local user's API credits.
		if origin := r.Header.Get("Origin"); origin != "" &&
			origin != "http://localhost:4003" && origin != "http://127.0.0.1:4003" &&
			origin != "http://localhost:4004" && origin != "http://127.0.0.1:4004" {
			writeError(w, http.StatusForbidden, "This API only accepts requests from the local Harper app.")
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (s *server) upstream(r *http.Request, method, path string, body io.Reader) (*http.Response, error) {
	req, err := http.NewRequestWithContext(r.Context(), method, s.baseURL+path, body)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+s.key)
	req.Header.Set("Content-Type", "application/json")
	return s.client.Do(req)
}

func (s *server) models(w http.ResponseWriter, r *http.Request) {
	if r.URL.Query().Get("refresh") != "1" {
		if result, ok := s.cachedModels(r.Context()); ok {
			writeJSON(w, 200, result)
			return
		}
	}
	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()
	resp, err := s.upstream(r.WithContext(ctx), http.MethodGet, "/models", nil)
	if err != nil {
		writeError(w, http.StatusBadGateway, "Could not reach DeepSeek. Check your connection and try again.")
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		s.upstreamError(w, resp)
		return
	}
	var result modelList
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&result); err != nil || len(result.Data) == 0 {
		writeError(w, http.StatusBadGateway, "DeepSeek returned an invalid model list. Try again.")
		return
	}
	s.cacheModels(r.Context(), result)
	writeJSON(w, http.StatusOK, result)
}

func (s *server) chat(w http.ResponseWriter, r *http.Request) {
	var files []*multipart.FileHeader
	var body io.Reader
	if strings.HasPrefix(r.Header.Get("Content-Type"), "multipart/form-data") {
		r.Body = http.MaxBytesReader(w, r.Body, 34<<20)
		if err := r.ParseMultipartForm(8 << 20); err != nil {
			writeError(w, 400, "Upload up to four images, no larger than 8 MB each.")
			return
		}
		defer r.MultipartForm.RemoveAll()
		files = r.MultipartForm.File["images"]
		if len(r.MultipartForm.File) > 1 || len(files) == 0 || len(files) > maxImagesPerTurn || len(r.FormValue("request")) > 2<<20 {
			writeError(w, 400, "Upload one to four images using the images field.")
			return
		}
		body = strings.NewReader(r.FormValue("request"))
	} else {
		r.Body = http.MaxBytesReader(w, r.Body, 2<<20)
		body = r.Body
	}
	decoder := json.NewDecoder(body)
	decoder.DisallowUnknownFields()
	var input chatRequest
	if err := decoder.Decode(&input); err != nil {
		writeError(w, http.StatusBadRequest, "Invalid chat request, or conversation is too large. Start a new chat.")
		return
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		writeError(w, http.StatusBadRequest, "Expected a single chat request.")
		return
	}
	if !modelID.MatchString(input.Model) || len(input.Messages) == 0 || len(input.Messages) > 200 {
		writeError(w, http.StatusBadRequest, "Select a model and send 1 to 200 messages. Start a new chat for longer conversations.")
		return
	}
	for _, m := range input.Messages {
		if (m.Role != "user" && m.Role != "assistant") || strings.TrimSpace(m.Content) == "" || len(m.Content) > 128<<10 {
			writeError(w, http.StatusBadRequest, "Messages must contain text and use a user or assistant role (maximum 128 KB each).")
			return
		}
	}
	if input.Messages[len(input.Messages)-1].Role != "user" {
		writeError(w, http.StatusBadRequest, "The last message must be from you.")
		return
	}
	if len(files)+len(input.AttachmentIDs) > maxImagesPerTurn || input.Regenerate && (len(files)+len(input.AttachmentIDs) > 0) {
		writeError(w, 400, "Attach up to four images to a new message; regenerate reuses the saved images.")
		return
	}
	seen := map[string]bool{}
	for _, id := range input.AttachmentIDs {
		if !modelID.MatchString(id) || seen[id] {
			writeError(w, 400, "Invalid or duplicate image attachment.")
			return
		}
		seen[id] = true
	}
	if len(files)+len(input.AttachmentIDs) > 0 {
		if s.store == nil || s.objects == nil {
			writeError(w, 503, "Image storage is unavailable.")
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
		supports, err := s.visionSupported(ctx, input.Model)
		cancel()
		if err != nil {
			writeError(w, 503, "Could not verify model image support. Please retry.")
			return
		}
		if !supports {
			writeError(w, 400, "Select an image-capable model, such as DeepSeek Flash.")
			return
		}
	}
	var uploaded []string
	claimed := false
	defer func() {
		if !claimed {
			for _, id := range uploaded {
				s.discardUpload(id)
			}
		}
	}()
	for _, file := range files {
		record, err := s.uploadImage(r.Context(), file)
		if err != nil {
			writeError(w, 400, err.Error())
			return
		}
		uploaded = append(uploaded, record.ID)
		input.AttachmentIDs = append(input.AttachmentIDs, record.ID)
	}
	var saved *conversation
	if s.store != nil {
		if input.ChatID == "" {
			input.ChatID = newID()
		}
		if !modelID.MatchString(input.ChatID) {
			writeError(w, 400, "Invalid conversation ID.")
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
		c, err := s.store.begin(ctx, input)
		cancel()
		if err != nil {
			switch {
			case errors.Is(err, errChatBusy):
				writeError(w, 409, "This conversation is already generating a reply. Wait for it to finish, then refresh.")
			case errors.Is(err, errCannotRetry), errors.Is(err, errChatTooLong), errors.Is(err, errInvalidAttachment):
				writeError(w, 400, err.Error())
			case errors.Is(err, gorm.ErrRecordNotFound):
				writeError(w, 404, "Conversation not found.")
			default:
				storageFailure(w)
			}
			return
		}
		saved = &c
		claimed = true
		// PostgreSQL owns the history, even if another browser sent stale text.
		input.Messages = nil
		for _, m := range c.Messages[:len(c.Messages)-1] {
			if strings.TrimSpace(m.Content) != "" && m.Error == "" {
				input.Messages = append(input.Messages, message{Role: m.Role, Content: m.Content})
			}
		}
	}
	answer := savedMessage{Role: "assistant", Status: "streaming"}
	if saved != nil {
		answer = saved.Messages[len(saved.Messages)-1]
	}
	finished := false
	defer func() {
		if saved == nil || finished {
			return
		}
		if answer.Status == "streaming" {
			answer.Status = "stopped"
		}
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := s.store.saveAnswer(ctx, saved.ID, answer); err != nil {
			log.Println("Could not persist interrupted reply; PostgreSQL is unavailable.")
		}
	}()
	if saved != nil {
		hasImages := false
		for _, msg := range saved.Messages {
			if len(msg.Attachments) > 0 {
				hasImages = true
			}
		}
		if hasImages {
			ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
			supports, err := s.visionSupported(ctx, input.Model)
			cancel()
			if err != nil || !supports {
				answer.Status, answer.Error = "error", "This conversation contains images. Select an image-capable model such as DeepSeek Flash."
				writeError(w, 400, answer.Error)
				return
			}
		}
	}
	upstreamMessages, err := s.upstreamMessages(r.Context(), input.Messages, saved)
	if err != nil {
		answer.Status, answer.Error = "error", err.Error()
		writeError(w, 400, answer.Error)
		return
	}
	thinking := "disabled"
	if input.Thinking {
		thinking = "enabled"
	}
	payload, err := json.Marshal(map[string]any{
		"model": input.Model, "messages": upstreamMessages, "stream": true,
		"thinking": map[string]string{"type": thinking},
	})
	if err != nil {
		writeError(w, http.StatusInternalServerError, "Could not prepare your message.")
		return
	}
	if len(payload) > 48<<20 {
		answer.Status, answer.Error = "error", "This conversation is too large. Start a new chat."
		writeError(w, 400, answer.Error)
		return
	}
	resp, err := s.upstream(r, http.MethodPost, "/chat/completions", bytes.NewReader(payload))
	if err != nil {
		if r.Context().Err() == nil {
			answer.Status, answer.Error = "error", "Could not reach DeepSeek. Check your connection and try again."
			writeError(w, http.StatusBadGateway, "Could not reach DeepSeek. Check your connection and try again.")
		}
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		answer.Status, answer.Error = "error", s.upstreamErrorMessage(resp)
		writeError(w, resp.StatusCode, answer.Error)
		return
	}
	if !strings.HasPrefix(resp.Header.Get("Content-Type"), "text/event-stream") {
		answer.Status, answer.Error = "error", "DeepSeek did not return a streaming response. Try again."
		writeError(w, http.StatusBadGateway, "DeepSeek did not return a streaming response. Try again.")
		return
	}
	w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	controller := http.NewResponseController(w)
	_ = controller.Flush()
	if saved != nil {
		data, _ := json.Marshal(map[string]any{"chat": saved})
		if _, err := fmt.Fprintf(w, "event: chat\ndata: %s\n\n", data); err != nil {
			return
		}
		_ = controller.Flush()
	}
	finished = s.forwardStream(w, r, resp.Body, saved, &answer)
}

func (s *server) upstreamError(w http.ResponseWriter, resp *http.Response) {
	text := s.upstreamErrorMessage(resp)
	status := resp.StatusCode
	if status < 400 || status > 599 {
		status = http.StatusBadGateway
	}
	writeError(w, status, text)
}

func (s *server) upstreamErrorMessage(resp *http.Response) string {
	var text string
	switch resp.StatusCode {
	case 401, 403:
		text = "DeepSeek rejected the API key. Check DEEPSEEK_API_KEY in .env, then run ./start.sh again."
	case 402:
		text = "Your DeepSeek balance is insufficient. Add credit to your DeepSeek account, then retry."
	case 429:
		text = "DeepSeek is receiving too many requests. Wait a moment and retry."
	case 400, 404, 422:
		var body struct {
			Error struct {
				Message string `json:"message"`
			} `json:"error"`
		}
		_ = json.NewDecoder(io.LimitReader(resp.Body, 16384)).Decode(&body)
		text = strings.ReplaceAll(body.Error.Message, s.key, "[redacted]")
		if text == "" {
			text = "DeepSeek could not accept this request. Try a different model or start a new chat."
		}
	default:
		text = "DeepSeek is temporarily unavailable. Please try again in a moment."
	}
	return text
}

func writeError(w http.ResponseWriter, status int, text string) {
	writeJSON(w, status, map[string]any{"error": map[string]string{"message": text}})
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}
