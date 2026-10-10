package main

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"time"
)

type modelList struct {
	Data []struct {
		ID              string   `json:"id"`
		Name            string   `json:"name,omitempty"`
		InputModalities []string `json:"input_modalities,omitempty"`
	} `json:"data"`
}

func (s *server) modelCacheKey() string {
	return fmt.Sprintf("harper:models:v2:%x", sha256.Sum256([]byte(s.baseURL+"\n"+s.key)))
}

func (s *server) cachedModels(ctx context.Context) (modelList, bool) {
	var result modelList
	if s.cache == nil {
		return result, false
	}
	ctx, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	data, err := s.cache.Get(ctx, s.modelCacheKey()).Bytes()
	if err != nil || json.Unmarshal(data, &result) != nil || len(result.Data) == 0 {
		return result, false
	}
	return result, true
}

func (s *server) cacheModels(ctx context.Context, models modelList) {
	if s.cache == nil {
		return
	}
	data, err := json.Marshal(models)
	if err != nil {
		return
	}
	ctx, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	_ = s.cache.Set(ctx, s.modelCacheKey(), data, 5*time.Minute).Err()
}
