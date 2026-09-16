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
	"os"
	"testing"

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
	if endpoint == "" && bucket == "" {
		t.Skip("Skipping S3 conformance test; neither S3_ENDPOINT nor S3_TEST_BUCKET set")
	}
	if bucket == "" {
		bucket = "test-bucket"
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
