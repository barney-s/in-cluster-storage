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

package blob

import (
	"bytes"
	"context"
	"crypto/sha256"
	"fmt"
	"io"
	"strings"
	"sync"
	"testing"
)

type testMemoryBackend struct {
	mu      sync.RWMutex
	objects map[string][]byte
}

func newTestMemoryBackend() *testMemoryBackend {
	return &testMemoryBackend{
		objects: make(map[string][]byte),
	}
}

func (m *testMemoryBackend) storageKey(volumeID, key string) string {
	if volumeID == "" || strings.HasPrefix(key, "volumes/") || strings.HasPrefix(key, "blobs/") {
		return key
	}
	return fmt.Sprintf("%s/%s", volumeID, key)
}

func (m *testMemoryBackend) PutObject(ctx context.Context, volumeID, key string, stream ByteStream) (string, error) {
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

func (m *testMemoryBackend) GetObject(ctx context.Context, volumeID, key string, offset, length int64, w io.Writer) error {
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

func (m *testMemoryBackend) DeleteObject(ctx context.Context, volumeID, key string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	k := m.storageKey(volumeID, key)
	delete(m.objects, k)
	return nil
}

func (m *testMemoryBackend) GetRedirectURL(ctx context.Context, volumeID, key string) (string, error) {
	return "", nil
}

func (m *testMemoryBackend) ListObjects(ctx context.Context, volumeID, prefix string) ([]string, error) {
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

func TestSingleBlobEncodeDecode(t *testing.T) {
	data := []byte(strings.Repeat("This is repetitive data to test compression. ", 100))
	stream := NewByteStreamFromBytes(data)
	prep, err := EncodeBlob(stream, true)
	if err != nil {
		t.Fatalf("EncodeBlob failed: %v", err)
	}

	encodedStream, err := EncodeLooseBlob(prep)
	if err != nil {
		t.Fatalf("EncodeLooseBlob failed: %v", err)
	}
	encoded, err := io.ReadAll(encodedStream)
	_ = encodedStream.Close()
	if err != nil {
		t.Fatalf("failed reading encoded loose blob: %v", err)
	}

	// Verify header
	if len(encoded) < SingleBlobHeaderSize {
		t.Fatalf("encoded blob too small: %d", len(encoded))
	}

	hdr, err := ParseFileHeader(encoded[:8])
	if err != nil {
		t.Fatalf("failed to parse file header: %v", err)
	}
	if hdr.FileType != FileTypeBlob {
		t.Fatalf("expected FileTypeBlob (1), got %d", hdr.FileType)
	}

	decStream, entry, err := DecodeLooseBlob(bytes.NewReader(encoded))
	if err != nil {
		t.Fatalf("failed to decode loose blob: %v", err)
	}

	decoded, err := io.ReadAll(decStream)
	_ = decStream.Close()
	if err != nil {
		t.Fatalf("failed reading decoded data: %v", err)
	}

	if entry.SHA256 != prep.SHA256 {
		t.Fatalf("SHA mismatch: %x vs %x", entry.SHA256, prep.SHA256)
	}

	if !bytes.Equal(decoded, data) {
		t.Fatalf("decoded data does not match original")
	}
}

func TestLargeTempFileStreaming(t *testing.T) {
	// Generate large content > 300KB to trigger tempfile creation in EncodeBlob
	largeData := []byte(strings.Repeat("0123456789abcdef", 25*1024)) // 400KB
	stream := NewByteStreamFromBytes(largeData)

	prep, err := EncodeBlob(stream, true)
	if err != nil {
		t.Fatalf("EncodeBlob failed: %v", err)
	}

	encodedStream, err := EncodeLooseBlob(prep)
	if err != nil {
		t.Fatalf("EncodeLooseBlob failed: %v", err)
	}

	decStream, entry, err := DecodeLooseBlob(encodedStream)
	if err != nil {
		t.Fatalf("DecodeLooseBlob failed: %v", err)
	}

	out, err := io.ReadAll(decStream)
	_ = decStream.Close()
	if err != nil {
		t.Fatalf("ReadAll failed: %v", err)
	}
	if !bytes.Equal(out, largeData) {
		t.Fatalf("Decoded data mismatch")
	}
	if entry.SHA256 != prep.SHA256 {
		t.Fatalf("SHA mismatch: %x vs %x", entry.SHA256, prep.SHA256)
	}
}

func TestPackfileEncodeDecode(t *testing.T) {
	origContents := [][]byte{
		[]byte("blob 1 content"),
		[]byte("blob 2 with some more text for testing"),
		[]byte(strings.Repeat("blob 3 highly repetitive repetitive data! ", 50)),
	}

	var preps []*EncodedBlob
	for _, c := range origContents {
		p, err := EncodeBlob(NewByteStreamFromBytes(c), true)
		if err != nil {
			t.Fatalf("EncodeBlob failed: %v", err)
		}
		preps = append(preps, p)
	}

	var packBuf bytes.Buffer
	packFile, err := WritePackfile(&packBuf, preps)
	if err != nil {
		t.Fatalf("WritePackfile failed: %v", err)
	}
	packData := packBuf.Bytes()

	// Verify pack tables
	parsedPack, err := DecodePackTable(bytes.NewReader(packData))
	if err != nil {
		t.Fatalf("failed to decode pack table: %v", err)
	}
	if len(parsedPack.Entries) != len(origContents) {
		t.Fatalf("expected %d entries, got %d", len(origContents), len(parsedPack.Entries))
	}

	// For each original input, verify lookup in table and streaming extraction from pack
	for _, orig := range origContents {
		targetSHA := sha256.Sum256(orig)
		entry, found := parsedPack.Lookup(targetSHA)
		if !found {
			t.Fatalf("SHA %x not found in pack entries", targetSHA)
		}

		payloadReader := bytes.NewReader(packData[entry.DataOffset:])
		blobStream, err := parsedPack.DecodeBlob(payloadReader, entry)
		if err != nil {
			t.Fatalf("failed to decode blob payload: %v", err)
		}
		if blobStream.Length() != int64(len(orig)) {
			t.Fatalf("length mismatch: got %d, expected %d", blobStream.Length(), len(orig))
		}
		extracted, err := io.ReadAll(blobStream)
		_ = blobStream.Close()
		if err != nil {
			t.Fatalf("failed reading extracted payload: %v", err)
		}
		if !bytes.Equal(extracted, orig) {
			t.Fatalf("extracted data mismatch for %s: got %q, expected %q", entry.SHA256Hex(), string(extracted), string(orig))
		}
	}

	_ = packFile
}

func TestBlobStoreOperations(t *testing.T) {
	ctx := t.Context()
	backend := newTestMemoryBackend()
	store := NewStore(backend, 500) // 500 bytes threshold for testing small vs large

	smallData1 := []byte("hello small blob 1")
	smallData2 := []byte("hello small blob 2")
	largeData := []byte(strings.Repeat("large data blob exceeding threshold ", 30)) // > 1000 bytes

	smallSHA1 := fmt.Sprintf("%x", sha256.Sum256(smallData1))
	smallSHA2 := fmt.Sprintf("%x", sha256.Sum256(smallData2))
	largeSHA := fmt.Sprintf("%x", sha256.Sum256(largeData))

	// 1. PutBlobs batch
	batch := map[string]ByteStream{
		"small1": NewByteStreamFromBytes(smallData1),
		"small2": NewByteStreamFromBytes(smallData2),
		"large":  NewByteStreamFromBytes(largeData),
	}
	if err := store.PutBlobs(ctx, batch); err != nil {
		t.Fatalf("PutBlobs failed: %v", err)
	}

	// 2. GetBlob check
	streamSmall, err := store.GetBlob(ctx, smallSHA1)
	if err != nil {
		t.Fatalf("failed to get small blob 1: %v", err)
	}
	gotSmall, _ := io.ReadAll(streamSmall)
	_ = streamSmall.Close()
	if !bytes.Equal(gotSmall, smallData1) {
		t.Fatalf("got small blob 1 mismatch: %q vs %q", string(gotSmall), string(smallData1))
	}

	streamSmall2, err := store.GetBlob(ctx, smallSHA2)
	if err != nil {
		t.Fatalf("failed to get small blob 2: %v", err)
	}
	gotSmall2, _ := io.ReadAll(streamSmall2)
	_ = streamSmall2.Close()
	if !bytes.Equal(gotSmall2, smallData2) {
		t.Fatalf("got small blob 2 mismatch: %q vs %q", string(gotSmall2), string(smallData2))
	}

	streamLarge, err := store.GetBlob(ctx, largeSHA)
	if err != nil {
		t.Fatalf("failed to get large blob: %v", err)
	}
	gotLarge, _ := io.ReadAll(streamLarge)
	_ = streamLarge.Close()
	if !bytes.Equal(gotLarge, largeData) {
		t.Fatalf("got large blob mismatch: %d vs %d", len(gotLarge), len(largeData))
	}

	// 3. Test recovery / fresh store pointing to same backend
	freshStore := NewStore(backend, 500)
	streamRecoveredSmall, err := freshStore.GetBlob(ctx, smallSHA1)
	if err != nil {
		t.Fatalf("fresh store failed to recover small blob: %v", err)
	}
	recSmall, _ := io.ReadAll(streamRecoveredSmall)
	_ = streamRecoveredSmall.Close()
	if !bytes.Equal(recSmall, smallData1) {
		t.Fatalf("recovered small blob mismatch: %q vs %q", string(recSmall), string(smallData1))
	}

	streamRecoveredLarge, err := freshStore.GetBlob(ctx, largeSHA)
	if err != nil {
		t.Fatalf("fresh store failed to recover large blob: %v", err)
	}
	recLarge, _ := io.ReadAll(streamRecoveredLarge)
	_ = streamRecoveredLarge.Close()
	if !bytes.Equal(recLarge, largeData) {
		t.Fatalf("recovered large blob mismatch: %d vs %d", len(recLarge), len(largeData))
	}
}
