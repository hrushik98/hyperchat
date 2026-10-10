package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

func (s *server) forwardStream(w http.ResponseWriter, r *http.Request, body io.Reader, chat *conversation, answer *savedMessage) bool {
	scanner := bufio.NewScanner(body)
	scanner.Buffer(make([]byte, 8192), 2<<20)
	controller := http.NewResponseController(w)
	lastSaved := time.Now()
	save := func() error {
		if chat == nil {
			return nil
		}
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		return s.store.saveAnswer(ctx, chat.ID, *answer)
	}
	streamError := func(text string) {
		answer.Status, answer.Error = "error", text
		data, _ := json.Marshal(map[string]any{"error": map[string]string{"message": text}})
		_, _ = fmt.Fprintf(w, "event: error\ndata: %s\n\n", data)
		_ = controller.Flush()
	}
	for scanner.Scan() {
		line := scanner.Text()
		if strings.HasPrefix(line, "data:") {
			data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
			if data == "[DONE]" {
				answer.Status = "complete"
				if strings.TrimSpace(answer.Content) == "" && chat != nil {
					streamError("DeepSeek returned an empty answer. Please retry.")
					return false
				}
				if err := save(); err != nil {
					streamError("Could not save this reply. Export the text and retry when storage is available.")
					return false
				}
				_, _ = fmt.Fprint(w, "data: [DONE]\n\n")
				_ = controller.Flush()
				return true
			}
			var event struct {
				Error *struct {
					Message string `json:"message"`
				} `json:"error"`
				Choices []struct {
					Delta struct {
						Content   string `json:"content"`
						Reasoning string `json:"reasoning_content"`
					} `json:"delta"`
					FinishReason string `json:"finish_reason"`
				} `json:"choices"`
			}
			if data != "" {
				if err := json.Unmarshal([]byte(data), &event); err != nil {
					streamError("DeepSeek returned an invalid stream. Please retry.")
					return false
				}
				if event.Error != nil {
					streamError(event.Error.Message)
					return false
				}
				if len(event.Choices) > 0 {
					choice := event.Choices[0]
					answer.Content += choice.Delta.Content
					answer.Reasoning += choice.Delta.Reasoning
					if choice.FinishReason == "length" {
						answer.Notice = "The response reached the model’s output limit. Ask it to continue."
					}
					if choice.FinishReason == "content_filter" {
						answer.Notice = "DeepSeek stopped this response due to its content policy."
					}
				}
			}
		}
		if _, err := fmt.Fprintln(w, line); err != nil {
			return false
		}
		if line == "" {
			if err := controller.Flush(); err != nil {
				return false
			}
			if chat != nil && time.Since(lastSaved) >= time.Second {
				if err := save(); err != nil {
					streamError("Could not save the reply. Check database availability and retry.")
					return false
				}
				lastSaved = time.Now()
			}
		}
	}
	if r.Context().Err() == nil {
		streamError("Connection interrupted. Please retry your message.")
	}
	return false
}
