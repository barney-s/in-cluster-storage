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

package controller

import (
	"github.com/gke-labs/in-cluster-storage/pkg/objectstore"
	"github.com/gke-labs/in-cluster-storage/pkg/objectstore/inmemorystorage"
)

// ObjectStorageBackend is an alias to objectstore.Backend for backwards compatibility.
type ObjectStorageBackend = objectstore.Backend

// MemoryBackend is an alias to inmemorystorage.Backend for backwards compatibility.
type MemoryBackend = inmemorystorage.Backend

// NewMemoryBackend creates a new in-memory object storage backend.
func NewMemoryBackend() *MemoryBackend {
	return inmemorystorage.New()
}
