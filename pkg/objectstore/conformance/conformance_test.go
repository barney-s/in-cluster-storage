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

package conformance

import (
	"bufio"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/gke-labs/in-cluster-storage/pkg/objectstore/filesystemstorage"
	"github.com/gke-labs/in-cluster-storage/pkg/objectstore/gcsstorage"
	"github.com/gke-labs/in-cluster-storage/pkg/objectstore/inmemorystorage"
	"github.com/gke-labs/in-cluster-storage/pkg/objectstore/s3storage"
)

func TestMemoryBackendConformance(t *testing.T) {
	b := inmemorystorage.New()
	RunBackendTests(t, b)
}

func TestFSBackendConformance(t *testing.T) {
	tempDir := t.TempDir()
	b, err := filesystemstorage.New(tempDir)
	if err != nil {
		t.Fatalf("Failed to initialize FSBackend: %v", err)
	}
	RunBackendTests(t, b)
}

func TestS3BackendConformance(t *testing.T) {
	endpoint := os.Getenv("S3_ENDPOINT")
	if endpoint == "" {
		endpoint = os.Getenv("AWS_ENDPOINT_URL")
	}
	bucket := os.Getenv("S3_TEST_BUCKET")
	if bucket == "" {
		bucket = "test-bucket"
	}
	if endpoint == "" && os.Getenv("S3_TEST_BUCKET") == "" {
		// No real S3 configured: run against the in-repo fake (fakes3/).
		endpoint = startFakeS3(t, bucket)
	}

	ctx := t.Context()
	b, err := s3storage.New(ctx, s3storage.Config{
		Bucket:       bucket,
		Prefix:       "conformance-test",
		Endpoint:     endpoint,
		Region:       "us-east-1",
		UsePathStyle: true,
	})
	if err != nil {
		t.Fatalf("Failed to initialize S3 backend: %v", err)
	}

	RunBackendTests(t, b)
}

func TestGCSBackendConformance(t *testing.T) {
	bucket := os.Getenv("GCS_TEST_BUCKET")
	if bucket == "" {
		t.Skip("Skipping GCS conformance test; GCS_TEST_BUCKET not set")
	}

	ctx := t.Context()
	b, err := gcsstorage.New(ctx, gcsstorage.Config{
		Bucket: bucket,
		Prefix: "conformance-test",
	})
	if err != nil {
		t.Fatalf("Failed to initialize GCS backend: %v", err)
	}

	RunBackendTests(t, b)
}

// startFakeS3 builds the fakes3 binary from the sibling module in this repo,
// starts it on an ephemeral port with the given bucket pre-created, and
// returns its endpoint URL. The process is stopped when the test finishes.
//
// fakes3 is a separate Go module, so it cannot be imported from here without
// a cross-module dependency; running the binary keeps the fake standalone.
func startFakeS3(t *testing.T, bucket string) string {
	t.Helper()

	goBin, err := exec.LookPath("go")
	if err != nil {
		t.Skip("go not found on PATH; cannot build fakes3 for the S3 conformance test")
	}

	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	fakes3Dir := filepath.Join(filepath.Dir(thisFile), "..", "..", "..", "fakes3")
	if _, err := os.Stat(filepath.Join(fakes3Dir, "go.mod")); err != nil {
		t.Fatalf("fakes3 module not found at %s: %v", fakes3Dir, err)
	}

	bin := filepath.Join(t.TempDir(), "fakes3")
	build := exec.Command(goBin, "build", "-o", bin, "./cmd/fakes3")
	build.Dir = fakes3Dir
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("building fakes3: %v\n%s", err, out)
	}

	cmd := exec.Command(bin, "--listen=127.0.0.1:0", "--buckets="+bucket, "--quiet")
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatalf("stdout pipe: %v", err)
	}
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		t.Fatalf("starting fakes3: %v", err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	})

	// The first stdout line is "fakes3 listening on http://127.0.0.1:PORT".
	addrCh := make(chan string, 1)
	go func() {
		scanner := bufio.NewScanner(stdout)
		for scanner.Scan() {
			line := scanner.Text()
			if _, addr, found := strings.Cut(line, "listening on "); found {
				addrCh <- strings.TrimSpace(addr)
				return
			}
		}
		addrCh <- ""
	}()
	var endpoint string
	select {
	case endpoint = <-addrCh:
	case <-time.After(30 * time.Second):
		t.Fatal("timed out waiting for fakes3 to report its address")
	}
	if endpoint == "" {
		t.Fatal("fakes3 exited before reporting its address")
	}

	// The SDK needs some credentials to sign with; fakes3 ignores them.
	if os.Getenv("AWS_ACCESS_KEY_ID") == "" {
		t.Setenv("AWS_ACCESS_KEY_ID", "fakes3")
		t.Setenv("AWS_SECRET_ACCESS_KEY", "fakes3")
	}
	return endpoint
}
