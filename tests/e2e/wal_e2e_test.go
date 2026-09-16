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

package e2e

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

func getPodUID(podName, namespace string) string {
	cmd := exec.Command("kubectl", "-n", namespace, "get", "pod", podName, "-o", "jsonpath={.metadata.uid}")
	out, err := cmd.CombinedOutput()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

func TestWALE2E(t *testing.T) {
	if os.Getenv("RUN_E2E") == "" {
		t.Skip("Skipping WAL E2E test; RUN_E2E not set")
	}

	h := NewHarness(t, "wal-e2e")
	h.Setup()

	gitRoot := h.GetGitRoot()
	experimentRoot := gitRoot

	// Build images
	h.DockerBuild("wal-buffer:e2e", filepath.Join(experimentRoot, "images/wal-buffer/Dockerfile"), experimentRoot)
	h.DockerBuild("wal-client-test:e2e", filepath.Join(experimentRoot, "images/wal-client-test/Dockerfile"), experimentRoot)

	// Load images into Kind
	h.KindLoad("wal-buffer:e2e")
	h.KindLoad("wal-client-test:e2e")

	// Read and adapt manifest to use hostPath for store directory to verify durability across pod restart
	manifestPath := filepath.Join(experimentRoot, "k8s/wal.yaml")
	b, err := os.ReadFile(manifestPath)
	if err != nil {
		t.Fatalf("Failed to read manifest: %v", err)
	}
	manifest := string(b)
	manifest = strings.ReplaceAll(manifest, "namespace: kube-objectfs-system", "namespace: default")
	manifest = strings.ReplaceAll(manifest, "image: wal-buffer:latest", "image: wal-buffer:e2e\n          imagePullPolicy: Never")

	// Replace store-dir emptyDir with a hostPath volume
	manifest = strings.ReplaceAll(manifest, "- name: store-dir\n          emptyDir: {}", `- name: store-dir
          hostPath:
            path: /tmp/wal-store-e2e
            type: DirectoryOrCreate`)

	// Apply WAL buffer manifests
	h.KubectlApplyContent("wal", manifest)

	// Wait for wal-buffer StatefulSet
	if err := h.WaitForStatefulSet("wal-buffer", "default", 2*time.Minute); err != nil {
		t.Logf("Events:\n%s\n", h.GetEvents("default"))
		t.Fatalf("WAL buffer failed to start: %v", err)
	}

	streamID1 := uuid.New().String()

	// Step 1: Run Client Pod 1 appending 10 records with witness ack and holding stream open across buffer restart
	clientPod1Yaml := fmt.Sprintf(`
apiVersion: v1
kind: Pod
metadata:
  name: wal-client-1
spec:
  restartPolicy: Never
  containers:
    - name: client
      image: wal-client-test:e2e
      imagePullPolicy: Never
      args:
        - "append"
        - "--dir=/data/wal"
        - "--stream-id=%s"
        - "--target=wal-buffer:50051"
        - "--count=10"
        - "--wait-level=witness"
        - "--hold-open=60s"
      volumeMounts:
        - name: wal-data
          mountPath: /data/wal
  volumes:
    - name: wal-data
      emptyDir: {}
`, streamID1)

	t.Logf("Creating Client Pod 1")
	h.KubectlApplyContent("wal-client-1", clientPod1Yaml)

	// Wait for Client 1 to start and perform initial appends
	time.Sleep(5 * time.Second)

	// Step 2: Delete buffer pod wal-buffer-0 while Client 1 is still running
	oldBufferUID := getPodUID("wal-buffer-0", "default")
	t.Logf("Deleting WAL buffer pod (old UID=%s) to test restart resilience with surviving client", oldBufferUID)
	h.DeletePod("wal-buffer-0", "default")

	// Wait for old buffer pod UID to disappear before waiting on readiness
	deadline := time.Now().Add(1 * time.Minute)
	for time.Now().Before(deadline) {
		currentUID := getPodUID("wal-buffer-0", "default")
		if currentUID != "" && currentUID != oldBufferUID {
			break
		}
		time.Sleep(500 * time.Millisecond)
	}

	if err := h.WaitForStatefulSet("wal-buffer", "default", 2*time.Minute); err != nil {
		t.Fatalf("Recreated WAL buffer failed to start: %v", err)
	}

	// Wait for Client 1 pod to finish holding open
	if err := waitForPodCompletion(h, "wal-client-1", "default", 2*time.Minute); err != nil {
		t.Logf("Events:\n%s\n", h.GetEvents("default"))
		t.Logf("Buffer Logs:\n%s\n", h.GetPodLogsByName("wal-buffer-0", "default"))
		t.Fatalf("Client Pod 1 failed: %v", err)
	}

	logs1 := h.GetPodLogsByName("wal-client-1", "default")
	t.Logf("Client 1 Logs: %s", logs1)
	if !strings.Contains(logs1, "SUCCESS") {
		t.Fatalf("Client 1 did not report SUCCESS: %s", logs1)
	}
	h.DeletePod("wal-client-1", "default")

	// Step 3: Run Client Pod 2 appending 10 more records with permanent ack
	streamID2 := uuid.New().String()
	clientPod2Yaml := fmt.Sprintf(`
apiVersion: v1
kind: Pod
metadata:
  name: wal-client-2
spec:
  restartPolicy: Never
  containers:
    - name: client
      image: wal-client-test:e2e
      imagePullPolicy: Never
      args:
        - "append"
        - "--dir=/data/wal"
        - "--stream-id=%s"
        - "--target=wal-buffer:50051"
        - "--count=10"
        - "--wait-level=permanent"
        - "--flush"
      volumeMounts:
        - name: wal-data
          mountPath: /data/wal
  volumes:
    - name: wal-data
      emptyDir: {}
`, streamID2)

	t.Logf("Creating Client Pod 2")
	h.KubectlApplyContent("wal-client-2", clientPod2Yaml)

	if err := waitForPodCompletion(h, "wal-client-2", "default", 2*time.Minute); err != nil {
		t.Logf("Buffer Logs:\n%s\n", h.GetPodLogsByName("wal-buffer-0", "default"))
		t.Fatalf("Client Pod 2 failed: %v", err)
	}

	logs2 := h.GetPodLogsByName("wal-client-2", "default")
	t.Logf("Client 2 Logs: %s", logs2)
	if !strings.Contains(logs2, "SUCCESS") {
		t.Fatalf("Client 2 did not report SUCCESS: %s", logs2)
	}
	h.DeletePod("wal-client-2", "default")

	// Step 4: Run Tail Pod to verify records from Client 1 (replayed after restart) and Client 2 appear
	tailPodYaml := `
apiVersion: v1
kind: Pod
metadata:
  name: wal-tail
spec:
  restartPolicy: Never
  containers:
    - name: tail
      image: wal-client-test:e2e
      imagePullPolicy: Never
      args:
        - "tail"
        - "--target=wal-buffer:50051"
        - "--from-pos=1"
        - "--count=20"
`
	t.Logf("Running Tail verification pod")
	h.KubectlApplyContent("wal-tail", tailPodYaml)

	if err := waitForPodCompletion(h, "wal-tail", "default", 2*time.Minute); err != nil {
		t.Logf("Tail Pod Logs:\n%s\n", h.GetPodLogsByName("wal-tail", "default"))
		t.Fatalf("Tail verification pod failed: %v", err)
	}

	tailLogs := h.GetPodLogsByName("wal-tail", "default")
	t.Logf("Tail Logs: %s", tailLogs)
	if !strings.Contains(tailLogs, "SUCCESS") {
		t.Fatalf("Tail did not succeed: %s", tailLogs)
	}

	h.DeletePod("wal-tail", "default")
	t.Logf("Successfully verified WAL E2E with buffer restart and replay!")
}

func TestWALMinIOE2E(t *testing.T) {
	if os.Getenv("RUN_E2E") == "" {
		t.Skip("Skipping WAL MinIO S3 E2E test; RUN_E2E not set")
	}

	h := NewHarness(t, "wal-s3-e2e")
	h.Setup()

	gitRoot := h.GetGitRoot()
	experimentRoot := gitRoot

	// Build images
	h.DockerBuild("wal-buffer:e2e", filepath.Join(experimentRoot, "images/wal-buffer/Dockerfile"), experimentRoot)
	h.DockerBuild("wal-client-test:e2e", filepath.Join(experimentRoot, "images/wal-client-test/Dockerfile"), experimentRoot)

	// Load images into Kind
	h.KindLoad("wal-buffer:e2e")
	h.KindLoad("wal-client-test:e2e")

	// 1. Deploy MinIO in Kind
	minioYaml := `
apiVersion: v1
kind: Service
metadata:
  name: minio
  namespace: default
spec:
  selector:
    app: minio
  ports:
    - port: 9000
      targetPort: 9000
      name: s3
---
apiVersion: apps/v1
kind: Deployment
metadata:
  name: minio
  namespace: default
spec:
  replicas: 1
  selector:
    matchLabels:
      app: minio
  template:
    metadata:
      labels:
        app: minio
    spec:
      containers:
        - name: minio
          image: quay.io/minio/minio:latest
          args:
            - "server"
            - "/data"
          env:
            - name: MINIO_ROOT_USER
              value: "minioadmin"
            - name: MINIO_ROOT_PASSWORD
              value: "minioadmin"
          ports:
            - containerPort: 9000
              name: s3
`
	h.KubectlApplyContent("minio", minioYaml)
	if err := h.WaitForDeployment("minio", "default", 2*time.Minute); err != nil {
		t.Fatalf("MinIO deployment failed to start: %v", err)
	}

	// 2. Initialize MinIO bucket using mc job
	mcJobYaml := `
apiVersion: batch/v1
kind: Job
metadata:
  name: minio-setup
  namespace: default
spec:
  template:
    spec:
      restartPolicy: OnFailure
      containers:
        - name: mc
          image: quay.io/minio/mc:latest
          command:
            - "/bin/sh"
            - "-c"
            - "until mc alias set local http://minio:9000 minioadmin minioadmin; do sleep 1; done; mc mb --ignore-existing local/wal-bucket"
`
	h.KubectlApplyContent("minio-setup", mcJobYaml)
	if err := h.WaitForJobSuccess("minio-setup", "default", 2*time.Minute); err != nil {
		t.Logf("MinIO logs:\n%s\n", h.GetPodLogs("app=minio", "default"))
		t.Logf("Events:\n%s\n", h.GetEvents("default"))
		t.Fatalf("MinIO bucket setup job failed: %v", err)
	}

	// 3. Deploy WAL Buffer configured with S3 backend pointing to MinIO
	walBufferS3Yaml := `
apiVersion: v1
kind: Service
metadata:
  name: wal-buffer
  namespace: default
spec:
  clusterIP: None
  selector:
    app: wal-buffer
  ports:
    - port: 50051
      targetPort: 50051
      name: grpc
---
apiVersion: apps/v1
kind: StatefulSet
metadata:
  name: wal-buffer
  namespace: default
spec:
  serviceName: wal-buffer
  replicas: 1
  selector:
    matchLabels:
      app: wal-buffer
  template:
    metadata:
      labels:
        app: wal-buffer
    spec:
      containers:
        - name: wal-buffer
          image: wal-buffer:e2e
          imagePullPolicy: Never
          args:
            - "--v=5"
            - "--port=50051"
            - "--data-dir=/data"
            - "--backend=s3://wal-bucket/e2e?endpoint=http://minio:9000&region=us-east-1"
          env:
            - name: AWS_ACCESS_KEY_ID
              value: "minioadmin"
            - name: AWS_SECRET_ACCESS_KEY
              value: "minioadmin"
            - name: AWS_REGION
              value: "us-east-1"
          ports:
            - containerPort: 50051
              name: grpc
          volumeMounts:
            - name: data-dir
              mountPath: /data
      volumes:
        - name: data-dir
          emptyDir: {}
`
	h.KubectlApplyContent("wal-s3", walBufferS3Yaml)
	if err := h.WaitForStatefulSet("wal-buffer", "default", 2*time.Minute); err != nil {
		t.Logf("Events:\n%s\n", h.GetEvents("default"))
		t.Fatalf("WAL buffer S3 failed to start: %v", err)
	}

	// 4. Run Client 1 appending records
	streamID1 := uuid.New().String()
	clientPod1Yaml := fmt.Sprintf(`
apiVersion: v1
kind: Pod
metadata:
  name: wal-client-s3-1
spec:
  restartPolicy: Never
  containers:
    - name: client
      image: wal-client-test:e2e
      imagePullPolicy: Never
      args:
        - "append"
        - "--dir=/data/wal"
        - "--stream-id=%s"
        - "--target=wal-buffer:50051"
        - "--count=10"
        - "--wait-level=witness"
        - "--hold-open=60s"
      volumeMounts:
        - name: wal-data
          mountPath: /data/wal
  volumes:
    - name: wal-data
      emptyDir: {}
`, streamID1)

	h.KubectlApplyContent("wal-client-s3-1", clientPod1Yaml)
	time.Sleep(5 * time.Second)

	// Step 5: Delete buffer pod to test restart with S3 backend
	oldBufferUID := getPodUID("wal-buffer-0", "default")
	t.Logf("Deleting WAL buffer pod (old UID=%s) with S3 backend", oldBufferUID)
	h.DeletePod("wal-buffer-0", "default")

	deadline := time.Now().Add(1 * time.Minute)
	for time.Now().Before(deadline) {
		currentUID := getPodUID("wal-buffer-0", "default")
		if currentUID != "" && currentUID != oldBufferUID {
			break
		}
		time.Sleep(500 * time.Millisecond)
	}

	if err := h.WaitForStatefulSet("wal-buffer", "default", 2*time.Minute); err != nil {
		t.Fatalf("Recreated WAL buffer failed to start: %v", err)
	}

	if err := waitForPodCompletion(h, "wal-client-s3-1", "default", 2*time.Minute); err != nil {
		t.Logf("Buffer Logs:\n%s\n", h.GetPodLogsByName("wal-buffer-0", "default"))
		t.Fatalf("Client Pod 1 S3 failed: %v", err)
	}
	h.DeletePod("wal-client-s3-1", "default")

	// Step 6: Run Client 2 appending 10 records with permanent ack
	streamID2 := uuid.New().String()
	clientPod2Yaml := fmt.Sprintf(`
apiVersion: v1
kind: Pod
metadata:
  name: wal-client-s3-2
spec:
  restartPolicy: Never
  containers:
    - name: client
      image: wal-client-test:e2e
      imagePullPolicy: Never
      args:
        - "append"
        - "--dir=/data/wal"
        - "--stream-id=%s"
        - "--target=wal-buffer:50051"
        - "--count=10"
        - "--wait-level=permanent"
        - "--flush"
      volumeMounts:
        - name: wal-data
          mountPath: /data/wal
  volumes:
    - name: wal-data
      emptyDir: {}
`, streamID2)

	h.KubectlApplyContent("wal-client-s3-2", clientPod2Yaml)
	if err := waitForPodCompletion(h, "wal-client-s3-2", "default", 2*time.Minute); err != nil {
		t.Logf("Buffer Logs:\n%s\n", h.GetPodLogsByName("wal-buffer-0", "default"))
		t.Fatalf("Client Pod 2 S3 failed: %v", err)
	}
	h.DeletePod("wal-client-s3-2", "default")

	// Step 7: Verify all 20 records with Tail pod
	tailPodYaml := `
apiVersion: v1
kind: Pod
metadata:
  name: wal-tail-s3
spec:
  restartPolicy: Never
  containers:
    - name: tail
      image: wal-client-test:e2e
      imagePullPolicy: Never
      args:
        - "tail"
        - "--target=wal-buffer:50051"
        - "--from-pos=1"
        - "--count=20"
`
	h.KubectlApplyContent("wal-tail-s3", tailPodYaml)
	if err := waitForPodCompletion(h, "wal-tail-s3", "default", 2*time.Minute); err != nil {
		t.Logf("Tail Pod Logs:\n%s\n", h.GetPodLogsByName("wal-tail-s3", "default"))
		t.Fatalf("Tail verification pod S3 failed: %v", err)
	}

	tailLogs := h.GetPodLogsByName("wal-tail-s3", "default")
	if !strings.Contains(tailLogs, "SUCCESS") {
		t.Fatalf("Tail S3 did not succeed: %s", tailLogs)
	}
	h.DeletePod("wal-tail-s3", "default")
	t.Logf("Successfully verified WAL E2E with S3/MinIO backend!")
}

func waitForPodCompletion(h *Harness, podName, namespace string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		phase := getPodPhase(podName, namespace)
		if phase == "Succeeded" {
			return nil
		}
		if phase == "Failed" {
			return fmt.Errorf("pod %s failed", podName)
		}
		time.Sleep(1 * time.Second)
	}
	return fmt.Errorf("timeout waiting for pod %s completion", podName)
}
