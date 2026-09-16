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

package objectstore

import (
	"testing"
)

func TestOpen(t *testing.T) {
	ctx := t.Context()

	// 1. Memory URL
	for _, rawURL := range []string{"", "memory", "memory://"} {
		b, err := Open(ctx, rawURL)
		if err != nil {
			t.Fatalf("Open(%q) failed: %v", rawURL, err)
		}
		if b == nil {
			t.Fatalf("Open(%q) returned nil backend", rawURL)
		}
	}

	// 2. File URL
	dir := t.TempDir()
	b, err := Open(ctx, "file://"+dir)
	if err != nil {
		t.Fatalf("Open(file://%s) failed: %v", dir, err)
	}
	if b == nil {
		t.Fatalf("Open(file) returned nil backend")
	}

	// 3. Invalid URL
	if _, err := Open(ctx, "unsupported://foo/bar"); err == nil {
		t.Fatalf("Expected error for unsupported scheme, got nil")
	}
}
