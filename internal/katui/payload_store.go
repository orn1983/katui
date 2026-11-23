package katui

import (
	"errors"
	"os"
	"sync"
)

const payloadQuota = 128 << 20

var errPayloadQuota = errors.New("complete payload unavailable: temporary storage limit reached")

type payloadStore struct {
	mu    sync.Mutex
	dir   string
	limit int64
	used  int64
	files map[string]int64
}

func newPayloadStore(dir string, limit int64) *payloadStore {
	return &payloadStore{dir: dir, limit: limit, files: make(map[string]int64)}
}

func (s *payloadStore) save(value []byte) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.files == nil {
		return "", os.ErrClosed
	}
	size := int64(len(value))
	if size > s.limit-s.used {
		return "", errPayloadQuota
	}
	f, err := os.CreateTemp(s.dir, "payload-*")
	if err != nil {
		return "", err
	}
	path := f.Name()
	// Keep a conservative reservation if both writing and cleanup fail.
	s.files[path] = size
	s.used += size
	_, writeErr := f.Write(value)
	if err = errors.Join(writeErr, f.Close()); err != nil {
		s.removeLocked(path)
		return "", err
	}
	return path, nil
}

func (s *payloadStore) remove(path string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.removeLocked(path)
}

func (s *payloadStore) removeLocked(path string) {
	if size, ok := s.files[path]; ok {
		if err := os.Remove(path); err == nil || errors.Is(err, os.ErrNotExist) {
			delete(s.files, path)
			s.used -= size
		}
	}
}

func (s *payloadStore) close() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.files = nil
	_ = os.RemoveAll(s.dir)
}
