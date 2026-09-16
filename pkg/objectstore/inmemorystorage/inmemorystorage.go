/*
Copyright 2026 Google LLC

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package inmemorystorage

import (
	"context"
	"crypto/sha256"
	"fmt"
	"io"
	"strings"
	"sync"

	"github.com/gke-labs/in-cluster-storage/pkg/objectfs/blob"
)

// Backend is an in-memory implementation of ObjectStorageBackend.
type Backend struct {
	mu      sync.RWMutex
	objects map[string][]byte // key: volumeID + "/" + key
}

// New creates a new in-memory object storage backend.
func New() *Backend {
	return &Backend{
		objects: make(map[string][]byte),
	}
}

// NewMemoryBackend creates a new in-memory object storage backend (alias for backwards compatibility).
func NewMemoryBackend() *Backend {
	return New()
}

func (m *Backend) storageKey(volumeID, key string) string {
	if volumeID == "" || strings.HasPrefix(key, "volumes/") || strings.HasPrefix(key, "blobs/") {
		return key
	}
	return fmt.Sprintf("%s/%s", volumeID, key)
}

func (m *Backend) PutObject(ctx context.Context, volumeID, key string, stream blob.ByteStream) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	k := m.storageKey(volumeID, key)
	if err := stream.Rewind(); err != nil {
		return "", err
	}
	buf, err := io.ReadAll(stream)
	if err != nil {
		return "", err
	}
	m.objects[k] = buf

	h := sha256.Sum256(buf)
	return fmt.Sprintf("%x", h), nil
}

func (m *Backend) GetObject(ctx context.Context, volumeID, key string, offset, length int64, w io.Writer) error {
	m.mu.RLock()
	defer m.mu.RUnlock()

	k := m.storageKey(volumeID, key)
	data, ok := m.objects[k]
	if !ok {
		return fmt.Errorf("object %s not found in volume %s", key, volumeID)
	}

	total := int64(len(data))
	if offset >= total {
		return nil
	}
	end := offset + length
	if length <= 0 || end > total {
		end = total
	}

	_, err := w.Write(data[offset:end])
	return err
}

func (m *Backend) DeleteObject(ctx context.Context, volumeID, key string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	k := m.storageKey(volumeID, key)
	delete(m.objects, k)
	return nil
}

func (m *Backend) GetRedirectURL(ctx context.Context, volumeID, key string) (string, error) {
	// For memory backend or when direct redirect is not used, returns empty string.
	return "", nil
}

func (m *Backend) ListObjects(ctx context.Context, volumeID, prefix string) ([]string, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	fullPrefix := m.storageKey(volumeID, prefix)
	var matches []string
	for k := range m.objects {
		if strings.HasPrefix(k, fullPrefix) {
			matches = append(matches, k)
		}
	}
	return matches, nil
}
