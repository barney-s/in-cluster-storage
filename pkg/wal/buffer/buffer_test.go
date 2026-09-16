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
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	pb "github.com/gke-labs/in-cluster-storage/pkg/api/wal/v1alpha1"
	"github.com/gke-labs/in-cluster-storage/pkg/objectfs/blob"
	"github.com/gke-labs/in-cluster-storage/pkg/objectfs/controller"
	"github.com/gke-labs/in-cluster-storage/pkg/wal"
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

func TestTailFlushedAndUnflushedMidStreamFlush(t *testing.T) {
	backend := controller.NewMemoryBackend()
	dataDir := t.TempDir()
	srv, addr, cleanup := startTestServer(t, backend, dataDir)
	defer cleanup()

	conn, err := grpc.NewClient(addr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatalf("dial failed: %v", err)
	}
	defer conn.Close()

	client := pb.NewWalBufferClient(conn)
	appendStream, err := client.Append(t.Context())
	if err != nil {
		t.Fatalf("failed to open append stream: %v", err)
	}

	streamID := uuid.New()
	_ = appendStream.Send(&pb.AppendRequest{Msg: &pb.AppendRequest_Hello{Hello: &pb.Hello{StreamId: streamID[:]}}})
	_, _ = appendStream.Recv()

	// 1. Append records 1..10
	for i := uint64(1); i <= 10; i++ {
		_ = appendStream.Send(&pb.AppendRequest{
			Msg: &pb.AppendRequest_Record{
				Record: &pb.AppendRecord{
					StreamSeq: i,
					Payload:   []byte(fmt.Sprintf("record-%03d", i)),
				},
			},
		})
		_, err := appendStream.Recv()
		if err != nil {
			t.Fatalf("failed to recv append ack: %v", err)
		}
	}

	// Flush 1..10 to permanent storage
	if _, err := client.Flush(t.Context(), &pb.FlushRequest{}); err != nil {
		t.Fatalf("failed to flush records 1..10: %v", err)
	}

	// 2. Append records 11..20 (committed locally, unflushed)
	for i := uint64(11); i <= 20; i++ {
		_ = appendStream.Send(&pb.AppendRequest{
			Msg: &pb.AppendRequest_Record{
				Record: &pb.AppendRecord{
					StreamSeq: i,
					Payload:   []byte(fmt.Sprintf("record-%03d", i)),
				},
			},
		})
		_, err := appendStream.Recv()
		if err != nil {
			t.Fatalf("failed to recv append ack: %v", err)
		}
	}

	// 3. Start Tail from position 1
	tailCtx, tailCancel := context.WithCancel(t.Context())
	defer tailCancel()

	tailStream, err := client.Tail(tailCtx, &pb.TailRequest{FromPosition: 1})
	if err != nil {
		t.Fatalf("failed to start tail: %v", err)
	}

	receivedPositions := make([]uint64, 0, 30)
	recvErrChan := make(chan error, 1)

	go func() {
		for len(receivedPositions) < 30 {
			resp, err := tailStream.Recv()
			if err != nil {
				recvErrChan <- err
				return
			}
			rec := resp.GetRecord()
			if rec != nil {
				receivedPositions = append(receivedPositions, rec.Position)
			}
		}
		recvErrChan <- nil
	}()

	// Wait until at least some records are received by Tail
	time.Sleep(50 * time.Millisecond)

	// 4. Trigger a flush mid-stream (flushing 11..20 to object storage)
	if _, err := client.Flush(t.Context(), &pb.FlushRequest{}); err != nil {
		t.Fatalf("failed mid-stream flush: %v", err)
	}

	// 5. Append records 21..30
	for i := uint64(21); i <= 30; i++ {
		_ = appendStream.Send(&pb.AppendRequest{
			Msg: &pb.AppendRequest_Record{
				Record: &pb.AppendRecord{
					StreamSeq: i,
					Payload:   []byte(fmt.Sprintf("record-%03d", i)),
				},
			},
		})
		_, err := appendStream.Recv()
		if err != nil {
			t.Fatalf("failed to recv append ack: %v", err)
		}
	}

	select {
	case err := <-recvErrChan:
		if err != nil {
			t.Fatalf("error receiving tail records: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatalf("timed out waiting for 30 tail records, got %d: %v", len(receivedPositions), receivedPositions)
	}

	if len(receivedPositions) != 30 {
		t.Fatalf("expected 30 records, got %d", len(receivedPositions))
	}
	for i, pos := range receivedPositions {
		if pos != uint64(i+1) {
			t.Errorf("expected position %d at index %d, got %d", i+1, i, pos)
		}
	}
	_ = srv
}

func TestTailMemoryBounded(t *testing.T) {
	backend := controller.NewMemoryBackend()
	dataDir := t.TempDir()

	ctx := t.Context()
	srv, err := NewServer(ctx, ServerConfig{
		Backend:        backend,
		DataDir:        dataDir,
		FlushInterval:  0,
		FlushBytes:     64 * 1024 * 1024,
		TailCacheBytes: 1024,
		BatchMaxDelay:  1 * time.Millisecond,
		BatchMaxSize:   64 * 1024,
	})
	if err != nil {
		t.Fatalf("failed to create server: %v", err)
	}
	defer srv.Close()

	streamID := uuid.New()
	st := srv.getOrCreateStream(streamID)

	const recordCount = 2000
	errCh := make(chan error, 1)
	go func() {
		for i := uint64(1); i <= recordCount; i++ {
			ackChan := make(chan ackResult, 1)
			srv.incomingChan <- incomingItem{
				streamID:  streamID,
				streamSeq: i,
				payload:   []byte("x"),
				ackChan:   ackChan,
			}
			res := <-ackChan
			if res.err != nil {
				errCh <- fmt.Errorf("commit failed at %d: %w", i, res.err)
				return
			}
		}
		errCh <- nil
	}()

	if err := <-errCh; err != nil {
		t.Fatalf("%v", err)
	}

	// Flush everything to permanent storage
	if _, err := srv.Flush(ctx, &pb.FlushRequest{}); err != nil {
		t.Fatalf("flush failed: %v", err)
	}

	// Verify server holds no in-memory record history
	srv.unflushedMu.Lock()
	unflushedLen := len(srv.unflushedRecords)
	srv.unflushedMu.Unlock()

	if unflushedLen != 0 {
		t.Errorf("expected 0 unflushed records in memory after flush, got %d", unflushedLen)
	}

	if st.witnessSeq != recordCount {
		t.Errorf("expected witness watermark %d, got %d", recordCount, st.witnessSeq)
	}
}

func TestRetentionPreservesUnflushedFiles(t *testing.T) {
	backend := controller.NewMemoryBackend()
	dataDir := t.TempDir()

	ctx := t.Context()
	srv, err := NewServer(ctx, ServerConfig{
		Backend:        backend,
		DataDir:        dataDir,
		FlushInterval:  10 * time.Second,
		FlushBytes:     256, // small segment files so rotation occurs
		TailCacheBytes: 0,   // prune flushed files aggressively
		BatchMaxDelay:  1 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("failed to create server: %v", err)
	}
	defer srv.Close()

	streamID := uuid.New()
	// Append 10 records
	for i := uint64(1); i <= 10; i++ {
		ackChan := make(chan ackResult, 1)
		srv.incomingChan <- incomingItem{
			streamID:  streamID,
			streamSeq: i,
			payload:   []byte(fmt.Sprintf("payload-long-string-to-cause-rotation-%d", i)),
			ackChan:   ackChan,
		}
		res := <-ackChan
		if res.err != nil {
			t.Fatalf("append %d failed: %v", i, res.err)
		}
	}

	// Flush the first 10 records
	if _, err := srv.Flush(ctx, &pb.FlushRequest{}); err != nil {
		t.Fatalf("flush failed: %v", err)
	}

	// Now append 10 MORE records that remain unflushed
	for i := uint64(11); i <= 20; i++ {
		ackChan := make(chan ackResult, 1)
		srv.incomingChan <- incomingItem{
			streamID:  streamID,
			streamSeq: i,
			payload:   []byte(fmt.Sprintf("payload-long-string-to-cause-rotation-%d", i)),
			ackChan:   ackChan,
		}
		res := <-ackChan
		if res.err != nil {
			t.Fatalf("append %d failed: %v", i, res.err)
		}
	}

	// Check disk: files containing records > 10 (unflushed) must still exist
	entries, err := os.ReadDir(dataDir)
	if err != nil {
		t.Fatalf("failed to read dataDir: %v", err)
	}

	var unflushedFileCount int
	for _, entry := range entries {
		if !entry.IsDir() && strings.HasSuffix(entry.Name(), ".wal") {
			filePath := filepath.Join(dataDir, entry.Name())
			_, meta, err := wal.ScanLogSegmentFile(filePath)
			if err != nil {
				t.Fatalf("failed to scan log segment %s: %v", filePath, err)
			}
			if meta.LastSeq > 10 {
				unflushedFileCount++
			}
		}
	}

	if unflushedFileCount == 0 {
		t.Fatalf("expected unflushed segment files to be preserved on disk")
	}
}

func TestRestartOnPersistentDataDir(t *testing.T) {
	backend := controller.NewMemoryBackend()
	dataDir := t.TempDir()

	ctx := t.Context()

	// 1. Incarnation 1: write and flush 10 records
	srv1, err := NewServer(ctx, ServerConfig{
		Backend:       backend,
		DataDir:       dataDir,
		FlushInterval: 10 * time.Second,
		BatchMaxDelay: 1 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("failed to create server 1: %v", err)
	}

	streamID := uuid.New()
	for i := uint64(1); i <= 10; i++ {
		ackChan := make(chan ackResult, 1)
		srv1.incomingChan <- incomingItem{
			streamID:  streamID,
			streamSeq: i,
			payload:   []byte(fmt.Sprintf("incarnation-1-rec-%d", i)),
			ackChan:   ackChan,
		}
		<-ackChan
	}

	if _, err := srv1.Flush(ctx, &pb.FlushRequest{}); err != nil {
		t.Fatalf("flush failed on srv1: %v", err)
	}
	_ = srv1.Close()

	// 2. Incarnation 2: reopen on the same dataDir
	srv2, addr2, cleanup2 := startTestServer(t, backend, dataDir)
	defer cleanup2()

	// Connect gRPC client to server 2
	conn, err := grpc.NewClient(addr2, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatalf("dial failed: %v", err)
	}
	defer conn.Close()

	client := pb.NewWalBufferClient(conn)

	// Append 5 new records in incarnation 2
	appendStream, err := client.Append(t.Context())
	if err != nil {
		t.Fatalf("append failed: %v", err)
	}

	_ = appendStream.Send(&pb.AppendRequest{Msg: &pb.AppendRequest_Hello{Hello: &pb.Hello{StreamId: streamID[:]}}})
	_, _ = appendStream.Recv()

	for i := uint64(11); i <= 15; i++ {
		_ = appendStream.Send(&pb.AppendRequest{
			Msg: &pb.AppendRequest_Record{
				Record: &pb.AppendRecord{
					StreamSeq: i,
					Payload:   []byte(fmt.Sprintf("incarnation-2-rec-%d", i)),
				},
			},
		})
		_, _ = appendStream.Recv()
	}

	// 3. Tail from position 1 on Server 2
	tailCtx, tailCancel := context.WithCancel(t.Context())
	defer tailCancel()

	tailStream, err := client.Tail(tailCtx, &pb.TailRequest{FromPosition: 1})
	if err != nil {
		t.Fatalf("tail failed: %v", err)
	}

	var receivedRecs []*pb.LogRecord
	for len(receivedRecs) < 15 {
		resp, err := tailStream.Recv()
		if err != nil {
			t.Fatalf("tail recv error: %v", err)
		}
		if resp.GetRecord() != nil {
			receivedRecs = append(receivedRecs, resp.GetRecord())
		}
	}

	if len(receivedRecs) != 15 {
		t.Fatalf("expected 15 records from tail, got %d", len(receivedRecs))
	}

	// Records 1..10 should have positions 1..10
	for i := 0; i < 10; i++ {
		if receivedRecs[i].Position != uint64(i+1) {
			t.Errorf("expected position %d at index %d, got %d", i+1, i, receivedRecs[i].Position)
		}
		if string(receivedRecs[i].Payload) != fmt.Sprintf("incarnation-1-rec-%d", i+1) {
			t.Errorf("payload mismatch at index %d: %s", i, string(receivedRecs[i].Payload))
		}
	}

	// Records 11..15 should have positions starting at srv2's position reservation (PositionFloorStep)
	for i := 10; i < 15; i++ {
		expectedPos := PositionFloorStep + uint64(i-10)
		if receivedRecs[i].Position != expectedPos {
			t.Errorf("expected position %d at index %d, got %d", expectedPos, i, receivedRecs[i].Position)
		}
		if string(receivedRecs[i].Payload) != fmt.Sprintf("incarnation-2-rec-%d", i+1) {
			t.Errorf("payload mismatch at index %d: %s", i, string(receivedRecs[i].Payload))
		}
	}
	_ = srv2
}
