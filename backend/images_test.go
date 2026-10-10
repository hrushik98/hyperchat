package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/minio/minio-go/v7"
	"gorm.io/gorm"
)

func testPNG(t *testing.T) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, 32, 32))
	for y := 0; y < 32; y++ {
		for x := 0; x < 32; x++ {
			img.Set(x, y, color.RGBA{R: 255, A: 255})
		}
	}
	var b bytes.Buffer
	if err := png.Encode(&b, img); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

func TestImageValidation(t *testing.T) {
	data := testPNG(t)
	typ, w, h, err := inspectImage(data)
	if err != nil || typ != "image/png" || w != 32 || h != 32 {
		t.Fatal("valid PNG was rejected")
	}
	for _, invalid := range [][]byte{nil, []byte("<svg xmlns='http://www.w3.org/2000/svg'></svg>"), make([]byte, maxImageBytes+1)} {
		if _, _, _, err := inspectImage(invalid); err == nil {
			t.Fatal("invalid or oversized image was accepted")
		}
	}
}

func imageRequest(t *testing.T, input chatRequest, files [][]byte) *http.Request {
	t.Helper()
	var b bytes.Buffer
	writer := multipart.NewWriter(&b)
	payload, _ := json.Marshal(input)
	_ = writer.WriteField("request", string(payload))
	for _, data := range files {
		file, err := writer.CreateFormFile("images", "photo.jpg")
		if err != nil {
			t.Fatal(err)
		}
		_, _ = file.Write(data)
	}
	_ = writer.Close()
	req := httptest.NewRequest("POST", "/api/chat", &b)
	req.Header.Set("Content-Type", writer.FormDataContentType())
	return req
}

func TestImagesPersistWithGORMAndS3(t *testing.T) {
	store := integrationStore(t)
	ctx := context.Background()
	objects, err := openObjects(ctx)
	if err != nil {
		t.Fatal("could not connect to object storage")
	}
	var histories [][]map[string]any
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/models" {
			io.WriteString(w, `{"data":[{"id":"deepseek-flash","input_modalities":["text","image"]},{"id":"deepseek-v4-pro","input_modalities":["text"]}]}`)
			return
		}
		var payload struct {
			Messages []map[string]any `json:"messages"`
		}
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Error(err)
			return
		}
		histories = append(histories, payload.Messages)
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"Red\"}}]}\n\ndata: [DONE]\n\n")
	}))
	defer upstream.Close()
	s := &server{store: store, objects: objects, key: "fake-key", baseURL: upstream.URL, client: upstream.Client()}
	t.Cleanup(func() {
		var records []imageAttachment
		_ = store.imageQuery(store.db).Find(&records).Error
		for _, record := range records {
			_ = objects.client.RemoveObject(ctx, objects.bucket, record.ObjectKey, minio.RemoveObjectOptions{})
		}
	})
	id := newID()
	input := chatRequest{ChatID: id, Model: "deepseek-flash", Messages: []message{{Role: "user", Content: "What color?"}}}
	data := testPNG(t)
	w := httptest.NewRecorder()
	localSecurity(s.applicationRoutes()).ServeHTTP(w, imageRequest(t, input, [][]byte{data, data}))
	if w.Code != 200 || !strings.Contains(w.Body.String(), "data: [DONE]") {
		t.Fatalf("upload failed: %d %s", w.Code, w.Body.String())
	}
	chat, err := store.get(ctx, id)
	if err != nil || len(chat.Messages[0].Attachments) != 2 {
		t.Fatal("image references did not persist")
	}
	refs := chat.Messages[0].Attachments
	var records []imageAttachment
	if err := store.imageQuery(store.db).Where(&imageAttachment{ChatID: id}).Find(&records).Error; err != nil || len(records) != 2 {
		t.Fatal("GORM did not associate both images with the chat")
	}
	t.Cleanup(func() {
		for _, rec := range records {
			_ = objects.client.RemoveObject(ctx, objects.bucket, rec.ObjectKey, minio.RemoveObjectOptions{})
		}
	})
	metadata, _ := json.Marshal(chat)
	if strings.Contains(string(metadata), "objectKey") || strings.Contains(string(metadata), "base64") {
		t.Fatal("private keys or image bytes leaked into chat JSON")
	}
	if refs[0].ContentType != "image/png" {
		t.Fatal("filename extension overrode detected content type")
	}
	parts := histories[0][0]["content"].([]any)
	url := parts[1].(map[string]any)["image_url"].(map[string]any)["url"].(string)
	decoded, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(url, "data:image/png;base64,"))
	if err != nil || !bytes.Equal(decoded, data) {
		t.Fatal("DeepSeek did not receive the original image bytes inline")
	}
	w = httptest.NewRecorder()
	localSecurity(s.applicationRoutes()).ServeHTTP(w, httptest.NewRequest("GET", "/api/images/"+refs[0].ID, nil))
	if w.Code != 200 || !bytes.Equal(w.Body.Bytes(), data) || w.Header().Get("Content-Type") != "image/png" {
		t.Fatal("saved image preview failed")
	}
	input.Messages[0].Content = "Look again"
	body, _ := json.Marshal(input)
	w = httptest.NewRecorder()
	localSecurity(s.applicationRoutes()).ServeHTTP(w, httptest.NewRequest("POST", "/api/chat", bytes.NewReader(body)))
	if w.Code != 200 || len(histories[1]) != 3 {
		t.Fatalf("follow-up lost image history: %d %s", w.Code, w.Body.String())
	}
	if _, ok := histories[1][0]["content"].([]any); !ok {
		t.Fatal("follow-up did not include saved images")
	}
	input.Regenerate = true
	body, _ = json.Marshal(input)
	w = httptest.NewRecorder()
	localSecurity(s.applicationRoutes()).ServeHTTP(w, httptest.NewRequest("POST", "/api/chat", bytes.NewReader(body)))
	if w.Code != 200 || len(histories[2]) != 3 {
		t.Fatal("regeneration did not preserve image history")
	}
	input.Regenerate = false
	input.Model = "deepseek-v4-pro"
	w = httptest.NewRecorder()
	localSecurity(s.applicationRoutes()).ServeHTTP(w, imageRequest(t, input, [][]byte{data}))
	if w.Code != 400 || len(histories) != 3 {
		t.Fatal("text-only model accepted an image upload")
	}
	input.Model = "deepseek-flash"
	w = httptest.NewRecorder()
	localSecurity(s.applicationRoutes()).ServeHTTP(w, imageRequest(t, input, [][]byte{[]byte("<svg/>")}))
	if w.Code != 400 {
		t.Fatal("invalid image bytes were accepted")
	}
	input.ChatID = newID()
	input.AttachmentIDs = []string{refs[0].ID}
	body, _ = json.Marshal(input)
	w = httptest.NewRecorder()
	localSecurity(s.applicationRoutes()).ServeHTTP(w, httptest.NewRequest("POST", "/api/chat", bytes.NewReader(body)))
	if w.Code != 400 {
		t.Fatal("an image was claimed by a second conversation")
	}
	if _, err := store.get(ctx, input.ChatID); err != gorm.ErrRecordNotFound {
		t.Fatal("failed attachment transaction left a conversation")
	}
	w = httptest.NewRecorder()
	localSecurity(s.applicationRoutes()).ServeHTTP(w, httptest.NewRequest("DELETE", "/api/chats/"+id, nil))
	if w.Code != 204 {
		t.Fatalf("chat deletion failed: %s", w.Body.String())
	}
	w = httptest.NewRecorder()
	localSecurity(s.applicationRoutes()).ServeHTTP(w, httptest.NewRequest("GET", "/api/images/"+refs[0].ID, nil))
	if w.Code != 404 {
		t.Fatal("deleted chat image remained accessible")
	}
	s.cleanImagesOnce(ctx)
	for _, record := range records {
		if _, err := objects.client.StatObject(ctx, objects.bucket, record.ObjectKey, minio.StatObjectOptions{}); err == nil {
			t.Fatal("deleted image remained in S3")
		}
	}
	var count int64
	if err := store.imageQuery(store.db).Count(&count).Error; err != nil || count != 0 {
		t.Fatal("cleanup did not remove image metadata")
	}
}

func TestAttachmentTransaction(t *testing.T) {
	store := integrationStore(t)
	record := imageAttachment{ID: newID(), Name: "photo.png", ContentType: "image/png", ObjectKey: "test/" + newID(), Ready: true, Size: 10}
	if err := store.imageQuery(store.db).Create(&record).Error; err != nil {
		t.Fatal(err)
	}
	input := chatRequest{ChatID: newID(), Model: "deepseek-flash", Messages: []message{{Role: "user", Content: "test"}}, AttachmentIDs: []string{record.ID}}
	chat, err := store.begin(context.Background(), input)
	if err != nil {
		t.Fatal(err)
	}
	if len(chat.Messages[0].Attachments) != 1 {
		t.Fatal("attachment transaction failed")
	}
}

// Opt-in smoke test: sends a generated red square to the real DeepSeek API.
// Chat records use temporary tables; no account or existing conversation changes.
func TestLiveVision(t *testing.T) {
	if os.Getenv("HARPER_LIVE_VISION_TESTS") != "1" {
		t.Skip("opt-in real API vision smoke test")
	}
	store := integrationStore(t)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	objects, err := openObjects(ctx)
	if err != nil {
		t.Fatal("object storage unavailable")
	}
	s := &server{store: store, objects: objects, key: os.Getenv("DEEPSEEK_API_KEY"), baseURL: os.Getenv("DEEPSEEK_BASE_URL"), client: &http.Client{Timeout: 60 * time.Second}}
	if s.baseURL == "" {
		s.baseURL = "https://api.deepseek.com"
	}
	t.Cleanup(func() {
		var records []imageAttachment
		_ = store.imageQuery(store.db).Find(&records).Error
		for _, rec := range records {
			_ = objects.client.RemoveObject(context.Background(), objects.bucket, rec.ObjectKey, minio.RemoveObjectOptions{})
		}
	})
	id := newID()
	data := testPNG(t)
	input := chatRequest{ChatID: id, Model: "deepseek-flash", Messages: []message{{Role: "user", Content: "What color is this image? Reply with one word."}}}
	w := httptest.NewRecorder()
	localSecurity(s.applicationRoutes()).ServeHTTP(w, imageRequest(t, input, [][]byte{data}).WithContext(ctx))
	if w.Code != 200 {
		t.Fatalf("live vision request failed: %d %s", w.Code, w.Body.String())
	}
	chat, err := store.get(ctx, id)
	if err != nil || len(chat.Messages) != 2 {
		t.Fatal("live image chat was not persisted")
	}
	if !strings.Contains(strings.ToLower(chat.Messages[1].Content), "red") {
		t.Fatalf("unexpected vision answer: %s", chat.Messages[1].Content)
	}
	t.Logf("DeepSeek identified the generated image: %s", strings.TrimSpace(chat.Messages[1].Content))
	// The optional probe coordinates an actual Docker service recreation from
	// outside the test container, then verifies both metadata and image bytes.
	if dir := os.Getenv("HARPER_RESTART_PROBE_DIR"); dir != "" {
		if err := os.WriteFile(filepath.Join(dir, "ready"), []byte("ready"), 0600); err != nil {
			t.Fatal(err)
		}
		for {
			if _, err := os.Stat(filepath.Join(dir, "recreated")); err == nil {
				break
			}
			select {
			case <-ctx.Done():
				t.Fatal("object restart probe timed out")
			case <-time.After(100 * time.Millisecond):
			}
		}
	}
	chat, err = store.get(ctx, id)
	if err != nil {
		t.Fatal("chat metadata did not survive object storage restart")
	}
	w = httptest.NewRecorder()
	localSecurity(s.applicationRoutes()).ServeHTTP(w, httptest.NewRequest("GET", "/api/images/"+chat.Messages[0].Attachments[0].ID, nil).WithContext(ctx))
	if w.Code != 200 || !bytes.Equal(w.Body.Bytes(), data) {
		t.Fatal("saved image bytes did not round trip after restart")
	}
}
