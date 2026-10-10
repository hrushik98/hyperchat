package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	_ "image/gif"
	_ "image/jpeg"
	_ "image/png"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"os"
	"path"
	"strings"
	"time"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
	_ "golang.org/x/image/webp"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

const maxImageBytes = 8 << 20
const maxImagesPerTurn = 4

var errInvalidAttachment = errors.New("an image upload is missing, unavailable, or already attached to another message")

type imageAttachment struct {
	ID          string `json:"id" gorm:"primaryKey;size:100"`
	Name        string `json:"name" gorm:"not null"`
	ContentType string `json:"contentType" gorm:"not null"`
	Size        int64  `json:"size" gorm:"not null"`
	Width       int    `json:"width"`
	Height      int    `json:"height"`
	ObjectKey   string `json:"-" gorm:"not null;uniqueIndex"`
	ChatID      string `json:"-" gorm:"index"`
	MessageID   string `json:"-"`
	CreatedAt   int64  `json:"-" gorm:"autoCreateTime:false;index"`
	Ready       bool   `json:"-" gorm:"not null;default:false"`
	DeletedAt   int64  `json:"-" gorm:"index"`
}

type objectStore struct {
	client *minio.Client
	bucket string
}

func jsonDecodeModels(reader io.Reader, list *modelList) error {
	return json.NewDecoder(io.LimitReader(reader, 1<<20)).Decode(list)
}

func openObjects(ctx context.Context) (*objectStore, error) {
	endpoint, key, secret := os.Getenv("S3_ENDPOINT"), os.Getenv("S3_ACCESS_KEY"), os.Getenv("S3_SECRET_KEY")
	if endpoint == "" || key == "" || secret == "" {
		return nil, errors.New("S3 endpoint or credentials are missing")
	}
	client, err := minio.New(endpoint, &minio.Options{Creds: credentials.NewStaticV4(key, secret, ""), Secure: os.Getenv("S3_USE_TLS") == "true", Region: "us-east-1", BucketLookup: minio.BucketLookupPath})
	if err != nil {
		return nil, err
	}
	bucket := os.Getenv("S3_BUCKET")
	if bucket == "" {
		bucket = "harper-images"
	}
	exists, err := client.BucketExists(ctx, bucket)
	if err != nil {
		return nil, err
	}
	if !exists {
		if err = client.MakeBucket(ctx, bucket, minio.MakeBucketOptions{Region: "us-east-1"}); err != nil {
			return nil, err
		}
	}
	return &objectStore{client: client, bucket: bucket}, nil
}

func (s *chatStore) imageQuery(db *gorm.DB) *gorm.DB {
	// Isolate the image model/table from the surrounding conversation transaction.
	db = db.Session(&gorm.Session{})
	if s.images != nil && s.images.Statement.Table != "" {
		return db.Table(s.images.Statement.Table).Model(&imageAttachment{})
	}
	return db.Model(&imageAttachment{})
}

func inspectImage(data []byte) (string, int, int, error) {
	if len(data) == 0 || len(data) > maxImageBytes {
		return "", 0, 0, errors.New("each image must be between 1 byte and 8 MB")
	}
	config, format, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil {
		return "", 0, 0, errors.New("use a valid JPEG, PNG, GIF, or WebP image")
	}
	types := map[string]string{"jpeg": "image/jpeg", "png": "image/png", "gif": "image/gif", "webp": "image/webp"}
	contentType, ok := types[format]
	if !ok || config.Width < 1 || config.Height < 1 || config.Width > 8192 || config.Height > 8192 {
		return "", 0, 0, errors.New("images must be JPEG, PNG, GIF, or WebP, with dimensions no larger than 8192 pixels per side")
	}
	return contentType, config.Width, config.Height, nil
}

func (s *server) uploadImage(ctx context.Context, file *multipart.FileHeader) (imageAttachment, error) {
	var record imageAttachment
	if s.objects == nil || s.store == nil {
		return record, errors.New("image storage is unavailable")
	}
	if file.Size > maxImageBytes {
		return record, errors.New("each image must be no larger than 8 MB")
	}
	reader, err := file.Open()
	if err != nil {
		return record, err
	}
	data, err := io.ReadAll(io.LimitReader(reader, maxImageBytes+1))
	reader.Close()
	if err != nil {
		return record, err
	}
	contentType, width, height, err := inspectImage(data)
	if err != nil {
		return record, err
	}
	name := path.Base(strings.ReplaceAll(file.Filename, "\\", "/"))
	name = strings.Map(func(r rune) rune {
		if r < 32 || r == 127 {
			return -1
		}
		return r
	}, name)
	nameRunes := []rune(name)
	if len(nameRunes) > 200 {
		name = string(nameRunes[:200])
	}
	record = imageAttachment{ID: newID(), Name: name, ContentType: contentType, Size: int64(len(data)), Width: width, Height: height, CreatedAt: time.Now().UnixMilli()}
	record.ObjectKey = "images/" + record.ID
	// Create metadata first. A crash leaves a recoverable pending record,
	// rather than an untracked object in the bucket.
	if err = s.store.imageQuery(s.store.db.WithContext(ctx)).Create(&record).Error; err != nil {
		return record, errors.New("could not save image metadata")
	}
	_, err = s.objects.client.PutObject(ctx, s.objects.bucket, record.ObjectKey, bytes.NewReader(data), record.Size, minio.PutObjectOptions{ContentType: contentType, DisableMultipart: true})
	if err == nil {
		record.Ready = true
		err = s.store.imageQuery(s.store.db.WithContext(ctx)).Where(&imageAttachment{ID: record.ID}).Update("Ready", true).Error
	}
	if err != nil {
		s.discardUpload(record.ID)
		return record, errors.New("could not store the image; please check object storage and retry")
	}
	return record, nil
}

func (s *server) discardUpload(id string) {
	if s.store == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = s.store.imageQuery(s.store.db.WithContext(ctx)).Where(map[string]any{"id": id, "chat_id": ""}).Update("DeletedAt", time.Now().UnixMilli()).Error
}

func (s *server) serveImage(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()
	var record imageAttachment
	err := s.store.imageQuery(s.store.db.WithContext(ctx)).Where(map[string]any{"id": r.PathValue("id"), "ready": true, "deleted_at": 0}).First(&record).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		writeError(w, 404, "Image not found.")
		return
	}
	if err != nil || s.objects == nil {
		writeError(w, 503, "Image storage is unavailable.")
		return
	}
	object, err := s.objects.client.GetObject(ctx, s.objects.bucket, record.ObjectKey, minio.GetObjectOptions{})
	if err != nil {
		writeError(w, 503, "Could not load this image.")
		return
	}
	defer object.Close()
	if _, err = object.Stat(); err != nil {
		writeError(w, 503, "Could not load this image.")
		return
	}
	w.Header().Set("Content-Type", record.ContentType)
	w.Header().Set("Content-Length", fmt.Sprint(record.Size))
	w.Header().Set("Content-Disposition", mime.FormatMediaType("inline", map[string]string{"filename": record.Name}))
	_, _ = io.CopyN(w, object, record.Size)
}

func (s *server) visionSupported(ctx context.Context, model string) (bool, error) {
	list, ok := s.cachedModels(ctx)
	if !ok {
		req, err := http.NewRequestWithContext(ctx, "GET", s.baseURL+"/models", nil)
		if err != nil {
			return false, err
		}
		req.Header.Set("Authorization", "Bearer "+s.key)
		resp, err := s.client.Do(req)
		if err != nil {
			return false, errors.New("could not verify model image support; please retry")
		}
		defer resp.Body.Close()
		if resp.StatusCode != 200 {
			return false, errors.New("could not verify model image support; refresh your connection")
		}
		if err := jsonDecodeModels(resp.Body, &list); err != nil {
			return false, err
		}
		s.cacheModels(ctx, list)
	}
	for _, item := range list.Data {
		if item.ID != model {
			continue
		}
		for _, mode := range item.InputModalities {
			if mode == "image" {
				return true, nil
			}
		}
		// Older /models responses omitted capabilities; this documented alias
		// supports vision. Explicit text-only metadata always takes precedence.
		return len(item.InputModalities) == 0 && item.ID == "deepseek-flash", nil
	}
	return false, nil
}

func (s *server) upstreamMessages(ctx context.Context, plain []message, chat *conversation) (any, error) {
	if chat == nil {
		return plain, nil
	}
	result := []map[string]any{}
	images, totalBytes := 0, int64(0)
	for _, msg := range chat.Messages[:len(chat.Messages)-1] {
		if msg.Error != "" || strings.TrimSpace(msg.Content) == "" && len(msg.Attachments) == 0 {
			continue
		}
		if len(msg.Attachments) == 0 {
			result = append(result, map[string]any{"role": msg.Role, "content": msg.Content})
			continue
		}
		if s.objects == nil {
			return nil, errors.New("image storage is unavailable")
		}
		parts := []map[string]any{{"type": "text", "text": msg.Content}}
		for _, ref := range msg.Attachments {
			images++
			totalBytes += ref.Size
			if images > 12 || totalBytes > 32<<20 {
				return nil, errors.New("this chat has too many or too-large images; start a new conversation (12 images and 32 MB total per chat)")
			}
			// Retrieve the trusted metadata instead of accepting browser URLs or keys.
			var stored imageAttachment
			err := s.store.imageQuery(s.store.db.WithContext(ctx)).Where(map[string]any{"id": ref.ID, "chat_id": chat.ID, "ready": true, "deleted_at": 0}).First(&stored).Error
			if err != nil {
				return nil, errors.New("an image in this chat is no longer available")
			}
			object, err := s.objects.client.GetObject(ctx, s.objects.bucket, stored.ObjectKey, minio.GetObjectOptions{})
			if err != nil {
				return nil, errors.New("could not read a saved image")
			}
			data, err := io.ReadAll(io.LimitReader(object, maxImageBytes+1))
			object.Close()
			if err != nil || int64(len(data)) != stored.Size || len(data) > maxImageBytes {
				return nil, errors.New("could not read a complete saved image")
			}
			url := "data:" + stored.ContentType + ";base64," + base64.StdEncoding.EncodeToString(data)
			parts = append(parts, map[string]any{"type": "image_url", "image_url": map[string]string{"url": url}})
		}
		result = append(result, map[string]any{"role": msg.Role, "content": parts})
	}
	return result, nil
}

func (s *server) cleanImagesOnce(ctx context.Context) {
	cleanupCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	var records []imageAttachment
	cutoff := time.Now().Add(-24 * time.Hour).UnixMilli()
	predicate := clause.Or(clause.Gt{Column: "deleted_at", Value: 0}, clause.And(clause.Eq{Column: "chat_id", Value: ""}, clause.Lt{Column: "created_at", Value: cutoff}))
	if err := s.store.imageQuery(s.store.db.WithContext(cleanupCtx)).Where(predicate).Limit(100).Find(&records).Error; err != nil {
		return
	}
	for _, record := range records {
		// Lock before marking an orphan, so a concurrent send can claim it safely.
		err := s.store.db.WithContext(cleanupCtx).Transaction(func(tx *gorm.DB) error {
			var current imageAttachment
			query := s.store.imageQuery(tx)
			if err := query.Clauses(clause.Locking{Strength: "UPDATE"}).First(&current, &imageAttachment{ID: record.ID}).Error; err != nil {
				return err
			}
			if current.DeletedAt == 0 && (current.ChatID != "" || current.CreatedAt >= cutoff) {
				return errors.New("image is still in use")
			}
			return s.store.imageQuery(tx).Where(&imageAttachment{ID: record.ID}).Update("DeletedAt", time.Now().UnixMilli()).Error
		})
		if err != nil {
			continue
		}
		if err = s.objects.client.RemoveObject(cleanupCtx, s.objects.bucket, record.ObjectKey, minio.RemoveObjectOptions{}); err != nil {
			continue
		}
		_ = s.store.imageQuery(s.store.db.WithContext(cleanupCtx)).Where(&imageAttachment{ID: record.ID}).Delete(&imageAttachment{}).Error
	}
}

func (s *server) cleanImages(ctx context.Context) {
	s.cleanImagesOnce(ctx)
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.cleanImagesOnce(ctx)
		}
	}
}
