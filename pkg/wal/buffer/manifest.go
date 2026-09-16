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

package buffer

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"

	"github.com/gke-labs/in-cluster-storage/pkg/objectfs/blob"
	"github.com/gke-labs/in-cluster-storage/pkg/wal"
)

const (
	ManifestKey = "wal/manifest.json"

	// PositionFloorStep is 2^32 (4294967296).
	PositionFloorStep uint64 = 1 << 32

	// FloorBumpThreshold is 2^31 (2147483648).
	FloorBumpThreshold uint64 = 1 << 31
)

// StreamState holds the persisted state of a single stream in the manifest.
type StreamState struct {
	S3AckedStreamSeq uint64 `json:"s3_acked_stream_seq"`
}

// Manifest represents the S3 WAL manifest (wal/manifest.json).
type Manifest struct {
	Segments      []string               `json:"segments"`
	LastPosition  uint64                 `json:"last_position"`
	PositionFloor uint64                 `json:"position_floor"`
	Streams       map[string]StreamState `json:"streams"`
}

// LoadManifest reads wal/manifest.json from the backend. If it does not exist, returns an empty Manifest.
func LoadManifest(ctx context.Context, backend blob.ObjectStorageBackend) (*Manifest, error) {
	var buf bytes.Buffer
	err := backend.GetObject(ctx, "", ManifestKey, 0, 0, &buf)
	if err != nil {
		return &Manifest{
			Segments:      nil,
			LastPosition:  0,
			PositionFloor: 0,
			Streams:       make(map[string]StreamState),
		}, nil
	}

	if buf.Len() == 0 {
		return &Manifest{
			Segments:      nil,
			LastPosition:  0,
			PositionFloor: 0,
			Streams:       make(map[string]StreamState),
		}, nil
	}

	var m Manifest
	if err := json.Unmarshal(buf.Bytes(), &m); err != nil {
		return nil, fmt.Errorf("failed to parse %s: %w", ManifestKey, err)
	}
	if m.Streams == nil {
		m.Streams = make(map[string]StreamState)
	}
	return &m, nil
}

// SaveManifest writes wal/manifest.json to the backend.
func SaveManifest(ctx context.Context, backend blob.ObjectStorageBackend, m *Manifest) error {
	data, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return fmt.Errorf("failed to marshal manifest: %w", err)
	}

	stream := blob.NewByteStreamFromBytes(data)
	defer stream.Close()

	if _, err := backend.PutObject(ctx, "", ManifestKey, stream); err != nil {
		return fmt.Errorf("failed to write %s to backend: %w", ManifestKey, err)
	}
	return nil
}

// ReadSegmentFromBackend reads and decodes all LogRecords from a segment path in object storage.
func ReadSegmentFromBackend(ctx context.Context, backend blob.ObjectStorageBackend, segPath string) ([]*wal.LogRecord, error) {
	var buf bytes.Buffer
	if err := backend.GetObject(ctx, "", segPath, 0, 0, &buf); err != nil {
		return nil, fmt.Errorf("failed to fetch segment %s: %w", segPath, err)
	}

	var records []*wal.LogRecord
	r := bytes.NewReader(buf.Bytes())
	for {
		rec, err := wal.DecodeLogRecord(r)
		if err != nil {
			if err == io.EOF {
				break
			}
			return nil, fmt.Errorf("failed to decode record in segment %s: %w", segPath, err)
		}
		records = append(records, rec)
	}
	return records, nil
}
