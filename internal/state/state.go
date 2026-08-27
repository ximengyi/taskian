package state

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

type Record struct {
	Status    string    `json:"status"`
	UpdatedAt time.Time `json:"updated_at"`
}

type Store struct {
	path      string
	Processed map[string]Record `json:"processed"`
	Fresh     bool              `json:"-"`
}

func Load(path string) (*Store, error) {
	store := &Store{path: path, Processed: make(map[string]Record)}
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		store.Fresh = true
		return store, nil
	}
	if err != nil {
		return nil, fmt.Errorf("读取状态文件: %w", err)
	}
	if err := json.Unmarshal(data, store); err != nil {
		return nil, fmt.Errorf("解析状态文件: %w", err)
	}
	store.path = path
	if store.Processed == nil {
		store.Processed = make(map[string]Record)
	}
	return store, nil
}

func (s *Store) Has(id string) bool {
	_, ok := s.Processed[id]
	return ok
}

func (s *Store) Mark(id, status string) {
	s.Processed[id] = Record{Status: status, UpdatedAt: time.Now()}
}

func (s *Store) Save() error {
	if err := os.MkdirAll(filepath.Dir(s.path), 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	temp := s.path + ".tmp"
	if err := os.WriteFile(temp, data, 0o600); err != nil {
		return err
	}
	if err := os.Rename(temp, s.path); err != nil {
		return err
	}
	s.Fresh = false
	return nil
}
