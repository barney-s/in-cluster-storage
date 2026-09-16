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
	"context"
	"errors"
	"fmt"
	"net"
	"testing"
	"time"

	pb "github.com/gke-labs/in-cluster-storage/pkg/api/wal/v1alpha1"
	"github.com/gke-labs/in-cluster-storage/pkg/objectfs/blob"
	"github.com/gke-labs/in-cluster-storage/pkg/objectfs/controller"
	"github.com/google/uuid"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

func startTestServer(t *testing.T, backend blob.ObjectStorageBackend, dataDir string) (*Server, string, func()) {
	ctx := t.Context()
	srv, err := NewServer(ctx, ServerConfig{
		Backend:       backend,
		DataDir:       dataDir,
		FlushInterval: 10 * time.Second,
		BatchMaxDelay: 2 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("failed to create server: %v", err)
	}

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("failed to listen: %v", err)
	}

	grpcServer := grpc.NewServer()
	pb.RegisterWalBufferServer(grpcServer, srv)

	go func() {
		_ = grpcServer.Serve(listener)
	}()

	cleanup := func() {
		grpcServer.Stop()
		_ = srv.Close()
		_ = listener.Close()
	}

	return srv, listener.Addr().String(), cleanup
}

func TestServerStartupAndPositionReservation(t *testing.T) {
	backend := controller.NewMemoryBackend()
	dataDir := t.TempDir()

	// 1. Initial startup on empty backend
	srv1, _, cleanup1 := startTestServer(t, backend, dataDir)
	if srv1.PositionFloor() != PositionFloorStep {
		t.Errorf("expected initial position_floor %d, got %d", PositionFloorStep, srv1.PositionFloor())
	}
	cleanup1()

	// 2. Second startup should advance position_floor by PositionFloorStep
	srv2, _, cleanup2 := startTestServer(t, backend, t.TempDir())
	if srv2.PositionFloor() != PositionFloorStep*2 {
		t.Errorf("expected second position_floor %d, got %d", PositionFloorStep*2, srv2.PositionFloor())
	}
	cleanup2()
}

func TestFloorBumpOnFlush(t *testing.T) {
	backend := controller.NewMemoryBackend()
	srv, addr, cleanup := startTestServer(t, backend, t.TempDir())
	defer cleanup()

	// Artificially advance lastPosition to within FloorBumpThreshold of PositionFloor
	srv.mu.Lock()
	srv.lastPosition = srv.positionFloor - 100
	srv.mu.Unlock()

	conn, err := grpc.NewClient(addr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatalf("dial failed: %v", err)
	}
	defer conn.Close()

	client := pb.NewWalBufferClient(conn)
	stream, err := client.Append(t.Context())
	if err != nil {
		t.Fatalf("append failed: %v", err)
	}

	streamID := uuid.New()
	_ = stream.Send(&pb.AppendRequest{Msg: &pb.AppendRequest_Hello{Hello: &pb.Hello{StreamId: streamID[:]}}})
	_, _ = stream.Recv()

	// Send a record
	_ = stream.Send(&pb.AppendRequest{Msg: &pb.AppendRequest_Record{Record: &pb.AppendRecord{StreamSeq: 1, Payload: []byte("test")}}})
	_, _ = stream.Recv()

	initialFloor := srv.PositionFloor()

	// Flush
	_, err = client.Flush(t.Context(), &pb.FlushRequest{})
	if err != nil {
		t.Fatalf("flush failed: %v", err)
	}

	if srv.PositionFloor() <= initialFloor {
		t.Errorf("expected position_floor to bump on flush (initial=%d, current=%d)", initialFloor, srv.PositionFloor())
	}
}

// faultyBackend wraps MemoryBackend and injects errors on PutObject for specific keys.
type faultyBackend struct {
	*controller.MemoryBackend
	failManifestPut bool
}

func (b *faultyBackend) PutObject(ctx context.Context, volumeID, key string, stream blob.ByteStream) (string, error) {
	if b.failManifestPut && key == ManifestKey {
		_ = stream.Close()
		return "", errors.New("injected PutObject error for manifest")
	}
	return b.MemoryBackend.PutObject(ctx, volumeID, key, stream)
}

func TestFloorBumpDurableAfterManifestSave(t *testing.T) {
	memBackend := controller.NewMemoryBackend()
	fb := &faultyBackend{
		MemoryBackend: memBackend,
	}

	srv, addr, cleanup := startTestServer(t, fb, t.TempDir())
	defer cleanup()

	// Artificially advance lastPosition to within FloorBumpThreshold of PositionFloor
	srv.mu.Lock()
	srv.lastPosition = srv.positionFloor - 100
	initialFloor := srv.positionFloor
	srv.mu.Unlock()

	conn, err := grpc.NewClient(addr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatalf("dial failed: %v", err)
	}
	defer conn.Close()

	client := pb.NewWalBufferClient(conn)
	stream, err := client.Append(t.Context())
	if err != nil {
		t.Fatalf("append failed: %v", err)
	}

	streamID := uuid.New()
	_ = stream.Send(&pb.AppendRequest{Msg: &pb.AppendRequest_Hello{Hello: &pb.Hello{StreamId: streamID[:]}}})
	_, _ = stream.Recv()

	_ = stream.Send(&pb.AppendRequest{Msg: &pb.AppendRequest_Record{Record: &pb.AppendRecord{StreamSeq: 1, Payload: []byte("test")}}})
	_, _ = stream.Recv()

	// Inject manifest failure
	fb.failManifestPut = true

	// Attempt flush - should fail
	_, err = client.Flush(t.Context(), &pb.FlushRequest{})
	if err == nil {
		t.Fatalf("expected flush to fail due to injected manifest error")
	}

	// Verify in-memory position floor was NOT bumped
	if srv.PositionFloor() != initialFloor {
		t.Errorf("expected position_floor to remain %d on failed manifest save, got %d", initialFloor, srv.PositionFloor())
	}
}

func TestAppendGroupCommitAndFlush(t *testing.T) {
	backend := controller.NewMemoryBackend()
	srv, addr, cleanup := startTestServer(t, backend, t.TempDir())
	defer cleanup()

	conn, err := grpc.NewClient(addr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatalf("dial failed: %v", err)
	}
	defer conn.Close()

	client := pb.NewWalBufferClient(conn)
	stream, err := client.Append(t.Context())
	if err != nil {
		t.Fatalf("failed to start stream: %v", err)
	}

	streamID := uuid.New()
	// 1. Handshake
	if err := stream.Send(&pb.AppendRequest{
		Msg: &pb.AppendRequest_Hello{
			Hello: &pb.Hello{
				StreamId: streamID[:],
			},
		},
	}); err != nil {
		t.Fatalf("failed to send hello: %v", err)
	}

	resp, err := stream.Recv()
	if err != nil {
		t.Fatalf("failed to recv hello ack: %v", err)
	}
	helloAck := resp.GetHelloAck()
	if helloAck == nil {
		t.Fatalf("unexpected hello ack: %+v", resp)
	}

	// 2. Append records
	for i := uint64(1); i <= 3; i++ {
		if err := stream.Send(&pb.AppendRequest{
			Msg: &pb.AppendRequest_Record{
				Record: &pb.AppendRecord{
					StreamSeq: i,
					Payload:   []byte(fmt.Sprintf("test-data-%d", i)),
				},
			},
		}); err != nil {
			t.Fatalf("failed to send record %d: %v", i, err)
		}

		ackResp, err := stream.Recv()
		if err != nil {
			t.Fatalf("failed to recv ack %d: %v", i, err)
		}
		ack := ackResp.GetAck()
		if ack == nil {
			t.Fatalf("expected Ack message, got: %+v", ackResp)
		}
		if ack.WitnessAckedStreamSeq != i {
			t.Errorf("expected WitnessAckedStreamSeq %d, got %d", i, ack.WitnessAckedStreamSeq)
		}
	}

	// 3. Flush to permanent storage
	flushResp, err := client.Flush(t.Context(), &pb.FlushRequest{})
	if err != nil {
		t.Fatalf("flush failed: %v", err)
	}
	if flushResp.LastPosition != 3 {
		t.Errorf("expected last_position 3, got %d", flushResp.LastPosition)
	}

	// Verify manifest in backend
	manifest, err := LoadManifest(t.Context(), backend)
	if err != nil {
		t.Fatalf("failed to load manifest: %v", err)
	}
	if len(manifest.Segments) != 1 {
		t.Fatalf("expected 1 segment in manifest, got %d", len(manifest.Segments))
	}
	if manifest.Streams[streamID.String()].S3AckedStreamSeq != 3 {
		t.Errorf("expected s3_acked_stream_seq 3, got %d", manifest.Streams[streamID.String()].S3AckedStreamSeq)
	}
	_ = srv
}
