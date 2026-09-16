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

package wal

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/uuid"
)

func TestClientRecordEncodeDecode(t *testing.T) {
	streamID := uuid.New()
	rec := &ClientRecord{
		StreamID:  streamID,
		StreamSeq: 42,
		Payload:   []byte("test-client-payload-1234"),
	}

	encoded := rec.Encode()
	if len(encoded) != ClientHeaderSize+len(rec.Payload) {
		t.Fatalf("expected encoded length %d, got %d", ClientHeaderSize+len(rec.Payload), len(encoded))
	}

	decoded, err := DecodeClientRecord(bytes.NewReader(encoded))
	if err != nil {
		t.Fatalf("failed to decode client record: %v", err)
	}

	if decoded.StreamID != rec.StreamID {
		t.Errorf("expected StreamID %s, got %s", rec.StreamID, decoded.StreamID)
	}
	if decoded.StreamSeq != rec.StreamSeq {
		t.Errorf("expected StreamSeq %d, got %d", rec.StreamSeq, decoded.StreamSeq)
	}
	if !bytes.Equal(decoded.Payload, rec.Payload) {
		t.Errorf("expected payload %q, got %q", string(rec.Payload), string(decoded.Payload))
	}
	if decoded.CRC32C != rec.CRC32C {
		t.Errorf("expected CRC32C %d, got %d", rec.CRC32C, decoded.CRC32C)
	}
}

func TestLogRecordEncodeDecode(t *testing.T) {
	streamID := uuid.New()
	rec := &LogRecord{
		Position:  100,
		StreamID:  streamID,
		StreamSeq: 42,
		Payload:   []byte("test-log-payload-1234"),
	}

	encoded := rec.Encode()
	if len(encoded) != LogHeaderSize+len(rec.Payload) {
		t.Fatalf("expected encoded length %d, got %d", LogHeaderSize+len(rec.Payload), len(encoded))
	}

	decoded, err := DecodeLogRecord(bytes.NewReader(encoded))
	if err != nil {
		t.Fatalf("failed to decode log record: %v", err)
	}

	if decoded.Position != rec.Position {
		t.Errorf("expected Position %d, got %d", rec.Position, decoded.Position)
	}
	if decoded.StreamID != rec.StreamID {
		t.Errorf("expected StreamID %s, got %s", rec.StreamID, decoded.StreamID)
	}
	if decoded.StreamSeq != rec.StreamSeq {
		t.Errorf("expected StreamSeq %d, got %d", rec.StreamSeq, decoded.StreamSeq)
	}
	if !bytes.Equal(decoded.Payload, rec.Payload) {
		t.Errorf("expected payload %q, got %q", string(rec.Payload), string(decoded.Payload))
	}
}

func TestRecordChecksumMismatch(t *testing.T) {
	streamID := uuid.New()
	rec := &ClientRecord{
		StreamID:  streamID,
		StreamSeq: 1,
		Payload:   []byte("hello world"),
	}

	encoded := rec.Encode()
	encoded[len(encoded)-1] ^= 0xFF

	_, err := DecodeClientRecord(bytes.NewReader(encoded))
	if err != ErrChecksumMismatch {
		t.Fatalf("expected ErrChecksumMismatch, got %v", err)
	}
}

func TestTornTrailingRecordDropAndRecoverFile(t *testing.T) {
	tmpDir := t.TempDir()
	store, _, err := NewClientSegmentStore(tmpDir, "client", 1024*1024)
	if err != nil {
		t.Fatalf("failed to create store: %v", err)
	}

	streamID := uuid.New()
	for i := uint64(1); i <= 5; i++ {
		rec := &ClientRecord{
			StreamID:  streamID,
			StreamSeq: i,
			Payload:   []byte("record payload"),
		}
		if err := store.Append(rec); err != nil {
			t.Fatalf("failed to append record %d: %v", i, err)
		}
	}
	if err := store.Close(); err != nil {
		t.Fatalf("failed to close store: %v", err)
	}

	// Corrupt file by appending partial header
	files, err := os.ReadDir(tmpDir)
	if err != nil {
		t.Fatalf("failed to read dir: %v", err)
	}
	if len(files) == 0 {
		t.Fatalf("expected at least one segment file")
	}

	targetFile := filepath.Join(tmpDir, files[0].Name())
	f, err := os.OpenFile(targetFile, os.O_WRONLY|os.O_APPEND, 0644)
	if err != nil {
		t.Fatalf("failed to open file: %v", err)
	}
	_, _ = f.Write([]byte("WALC\x00\x01\x02\x03\x04\x05"))
	_ = f.Close()

	// Reopen store: torn trailing record dropped and .recover backup created
	reopenedStore, records, err := NewClientSegmentStore(tmpDir, "client", 1024*1024)
	if err != nil {
		t.Fatalf("failed to reopen store: %v", err)
	}
	defer reopenedStore.Close()

	if len(records) != 5 {
		t.Fatalf("expected 5 recovered records, got %d", len(records))
	}

	// Verify .recover file was created
	entries, _ := os.ReadDir(tmpDir)
	var foundRecover bool
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".recover") {
			foundRecover = true
			break
		}
	}
	if !foundRecover {
		t.Errorf("expected .recover backup file to be created upon truncation")
	}
}
