package main

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
	"gorm.io/gorm/logger"
)

var errChatBusy = errors.New("this conversation is already generating a reply")
var errCannotRetry = errors.New("there is no reply to regenerate")
var errChatTooLong = errors.New("conversation is too long; start a new chat")

type savedMessage struct {
	ID          string            `json:"id"`
	Role        string            `json:"role"`
	Content     string            `json:"content"`
	Reasoning   string            `json:"reasoning,omitempty"`
	Model       string            `json:"model,omitempty"`
	Status      string            `json:"status,omitempty"`
	Error       string            `json:"error,omitempty"`
	Notice      string            `json:"notice,omitempty"`
	Attachments []imageAttachment `json:"attachments,omitempty"`
}

type conversation struct {
	ID        string         `json:"id" gorm:"primaryKey;size:100"`
	Title     string         `json:"title" gorm:"not null"`
	Model     string         `json:"model" gorm:"not null"`
	Thinking  bool           `json:"thinking" gorm:"not null;default:false"`
	UpdatedAt int64          `json:"updatedAt" gorm:"autoUpdateTime:false;not null;index"`
	Messages  []savedMessage `json:"messages" gorm:"type:jsonb;serializer:json;not null"`
}

type chatStore struct {
	db     *gorm.DB
	images *gorm.DB
}

func openStore(ctx context.Context) (*chatStore, error) {
	// The PostgreSQL driver reads the PG* environment variables. Silence query
	// logs so transcripts and database credentials never appear in app logs.
	db, err := gorm.Open(postgres.Open(""), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		return nil, err
	}
	sqlDB, err := db.DB()
	if err != nil {
		return nil, err
	}
	sqlDB.SetMaxOpenConns(10)
	sqlDB.SetMaxIdleConns(5)
	sqlDB.SetConnMaxLifetime(time.Hour)
	s := &chatStore{db: db, images: db.Model(&imageAttachment{})}
	if err = sqlDB.PingContext(ctx); err == nil {
		err = db.WithContext(ctx).AutoMigrate(&conversation{}, &imageAttachment{})
	}
	if err == nil {
		// A backend restart interrupts any outstanding generations.
		var chats []conversation
		err = db.WithContext(ctx).Find(&chats).Error
		for _, c := range chats {
			changed := false
			for i := range c.Messages {
				if c.Messages[i].Status == "streaming" {
					c.Messages[i].Status = "stopped"
					changed = true
				}
			}
			if changed && err == nil {
				err = db.WithContext(ctx).Model(&c).Select("Messages").Updates(&c).Error
			}
		}
	}
	if err != nil {
		sqlDB.Close()
		return nil, err
	}
	return s, nil
}

func (s *chatStore) ping(ctx context.Context) error {
	db, err := s.db.DB()
	if err != nil {
		return err
	}
	return db.PingContext(ctx)
}
func (s *chatStore) close() {
	if db, err := s.db.DB(); err == nil {
		_ = db.Close()
	}
}
func (s *chatStore) list(ctx context.Context) ([]conversation, error) {
	chats := []conversation{}
	err := s.db.WithContext(ctx).Order("updated_at DESC").Order("id").Find(&chats).Error
	return chats, err
}
func (s *chatStore) get(ctx context.Context, id string) (conversation, error) {
	var c conversation
	err := s.db.WithContext(ctx).First(&c, &conversation{ID: id}).Error
	return c, err
}
func newID() string { return rand.Text() }

func (s *chatStore) begin(ctx context.Context, input chatRequest) (conversation, error) {
	var c conversation
	err := s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		content := input.Messages[len(input.Messages)-1].Content
		title := []rune(strings.TrimSpace(content))
		if len(title) > 54 {
			title = title[:54]
		}
		if !input.Regenerate {
			initial := conversation{ID: input.ChatID, Title: string(title), Model: input.Model, Thinking: input.Thinking, UpdatedAt: time.Now().UnixMilli(), Messages: []savedMessage{}}
			if err := tx.Clauses(clause.OnConflict{DoNothing: true}).Create(&initial).Error; err != nil {
				return err
			}
		}
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).First(&c, &conversation{ID: input.ChatID}).Error; err != nil {
			return err
		}
		if len(c.Messages) > 0 && c.Messages[len(c.Messages)-1].Status == "streaming" {
			return errChatBusy
		}
		if input.Regenerate {
			if len(c.Messages) < 2 || c.Messages[len(c.Messages)-1].Role != "assistant" || c.Messages[len(c.Messages)-2].Role != "user" {
				return errCannotRetry
			}
			c.Messages = c.Messages[:len(c.Messages)-1]
		} else {
			user := savedMessage{ID: newID(), Role: "user", Content: content}
			for _, id := range input.AttachmentIDs {
				var image imageAttachment
				query := s.imageQuery(tx)
				if err := query.Clauses(clause.Locking{Strength: "UPDATE"}).First(&image, &imageAttachment{ID: id}).Error; err != nil {
					return errInvalidAttachment
				}
				if !image.Ready || image.ChatID != "" || image.DeletedAt != 0 {
					return errInvalidAttachment
				}
				image.ChatID, image.MessageID = c.ID, user.ID
				if err := s.imageQuery(tx).Where(&imageAttachment{ID: image.ID}).Select("ChatID", "MessageID").Updates(&image).Error; err != nil {
					return err
				}
				user.Attachments = append(user.Attachments, image)
			}
			c.Messages = append(c.Messages, user)
		}
		if len(c.Messages) >= 200 {
			return errChatTooLong
		}
		c.Messages = append(c.Messages, savedMessage{ID: newID(), Role: "assistant", Model: input.Model, Status: "streaming"})
		c.Model, c.Thinking, c.UpdatedAt = input.Model, input.Thinking, time.Now().UnixMilli()
		return tx.Model(&c).Select("Model", "Thinking", "UpdatedAt", "Messages").Updates(&c).Error
	})
	return c, err
}

func (s *chatStore) saveAnswer(ctx context.Context, id string, answer savedMessage) error {
	return s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var c conversation
		err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).First(&c, &conversation{ID: id}).Error
		// Deletes cannot be undone by a late stream write, and an old response
		// cannot overwrite a newer reply or independently changed settings.
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil
		}
		if err != nil {
			return err
		}
		if len(c.Messages) == 0 || c.Messages[len(c.Messages)-1].ID != answer.ID {
			return nil
		}
		c.Messages[len(c.Messages)-1] = answer
		c.UpdatedAt = time.Now().UnixMilli()
		return tx.Model(&c).Select("Messages", "UpdatedAt").Updates(&c).Error
	})
}

func (s *server) listChats(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	chats, err := s.store.list(ctx)
	if err != nil {
		storageFailure(w)
		return
	}
	writeJSON(w, 200, map[string]any{"chats": chats})
}
func (s *server) getChat(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	c, err := s.store.get(ctx, r.PathValue("id"))
	if errors.Is(err, gorm.ErrRecordNotFound) {
		writeError(w, 404, "Conversation not found.")
		return
	}
	if err != nil {
		storageFailure(w)
		return
	}
	writeJSON(w, 200, c)
}
func (s *server) deleteChat(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	id := r.PathValue("id")
	err := s.store.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		query := s.store.imageQuery(tx)
		if err := query.Where(&imageAttachment{ChatID: id}).Update("DeletedAt", time.Now().UnixMilli()).Error; err != nil {
			return err
		}
		return tx.Delete(&conversation{}, &conversation{ID: id}).Error
	})
	if err != nil {
		storageFailure(w)
		return
	}
	w.WriteHeader(204)
}
func (s *server) updateChat(w http.ResponseWriter, r *http.Request) {
	var input struct {
		Model    string `json:"model"`
		Thinking bool   `json:"thinking"`
	}
	r.Body = http.MaxBytesReader(w, r.Body, 4096)
	if err := json.NewDecoder(r.Body).Decode(&input); err != nil || !modelID.MatchString(input.Model) {
		writeError(w, 400, "Invalid conversation settings.")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	c := conversation{ID: r.PathValue("id"), Model: input.Model, Thinking: input.Thinking}
	result := s.store.db.WithContext(ctx).Model(&c).Select("Model", "Thinking").Updates(&c)
	if result.Error != nil {
		storageFailure(w)
		return
	}
	if result.RowsAffected == 0 {
		writeError(w, 404, "Conversation not found.")
		return
	}
	w.WriteHeader(204)
}
func (s *server) importChats(w http.ResponseWriter, r *http.Request) {
	var input struct {
		Chats []conversation `json:"chats"`
	}
	r.Body = http.MaxBytesReader(w, r.Body, 16<<20)
	if err := json.NewDecoder(r.Body).Decode(&input); err != nil || len(input.Chats) > 1000 {
		writeError(w, 400, "Invalid browser history, or import is too large.")
		return
	}
	for i := range input.Chats {
		c := &input.Chats[i]
		if !modelID.MatchString(c.ID) || !modelID.MatchString(c.Model) || len(c.Title) > 1000 || len(c.Messages) > 1000 {
			writeError(w, 400, "Browser history contains an invalid conversation.")
			return
		}
		if c.Messages == nil {
			c.Messages = []savedMessage{}
		}
		for j := range c.Messages {
			m := &c.Messages[j]
			if len(m.Attachments) != 0 {
				writeError(w, 400, "Browser imports cannot reference server image uploads.")
				return
			}
			if m.Role != "user" && m.Role != "assistant" {
				writeError(w, 400, "Invalid message in browser history.")
				return
			}
			if m.ID == "" {
				m.ID = newID()
			}
			if m.Status == "streaming" {
				m.Status = "stopped"
			}
		}
		if c.UpdatedAt <= 0 {
			c.UpdatedAt = time.Now().UnixMilli()
		}
	}
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()
	imported := int64(0)
	err := s.store.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		for _, c := range input.Chats {
			result := tx.Clauses(clause.OnConflict{DoNothing: true}).Create(&c)
			if result.Error != nil {
				return result.Error
			}
			imported += result.RowsAffected
		}
		return nil
	})
	if err != nil {
		storageFailure(w)
		return
	}
	writeJSON(w, 200, map[string]any{"imported": imported})
}
func storageFailure(w http.ResponseWriter) {
	writeError(w, 503, "Chat storage is unavailable. Please retry in a moment.")
}
