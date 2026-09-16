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

package client

import (
	"context"
	"fmt"
	"net"
	"os"
	"sync"
	"testing"
	"time"

	pb "github.com/gke-labs/in-cluster-storage/pkg/api/wal/v1alpha1"
	"github.com/gke-labs/in-cluster-storage/pkg/objectfs/controller"
	"github.com/gke-labs/in-cluster-storage/pkg/wal/buffer"
	"github.com/google/uuid"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

type testServerHandle struct {
	srv        *buffer.Server
	addr       string
	grpcServer *grpc.Server
	listener   net.Listener
}

func (h *testServerHandle) StopGraceful() {
	h.grpcServer.Stop()
	_ = h.srv.Close()
	_ = h.listener.Close()
}

// StopWithoutClose stops the gRPC listener and server WITHOUT calling srv.Close(),
// simulating an ungraceful crash where no final flush occurs.
func (h *testServerHandle) StopWithoutClose() {
	h.grpcServer.Stop()
	_ = h.listener.Close()
}

func startBufferServer(t *testing.T, backend *controller.MemoryBackend, dataDir string) *testServerHandle {
	ctx := t.Context()
	srv, err := buffer.NewServer(ctx, buffer.ServerConfig{
		Backend:       backend,
		DataDir:       dataDir,
		FlushInterval: 10 * time.Second,
		BatchMaxDelay: 2 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("failed to start buffer server: %v", err)
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

	return &testServerHandle{
		srv:        srv,
		addr:       listener.Addr().String(),
		grpcServer: grpcServer,
		listener:   listener,
	}
}

// 1. Append, witness ack, then permanent ack after Flush; local file deleted only after permanent ack.
func TestAppendWitnessAndPermanentAck(t *testing.T) {
	backend := controller.NewMemoryBackend()
	handle := startBufferServer(t, backend, t.TempDir())
	defer handle.StopGraceful()

	clientDir := t.TempDir()
	streamID := uuid.New()

	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()

	// Use small maxSegmentSize so segments rotate quickly
	stream, err := Open(ctx, clientDir, streamID, handle.addr, WithMaxSegmentSize(100))
	if err != nil {
		t.Fatalf("failed to open client stream: %v", err)
	}
	defer stream.Close()

	// Append 10 records
	for i := 1; i <= 10; i++ {
		seq, err := stream.Append(ctx, []byte(fmt.Sprintf("data-%d", i)))
		if err != nil {
			t.Fatalf("failed to append record %d: %v", i, err)
		}
		if err := stream.Wait(ctx, seq, Witness, false); err != nil {
			t.Fatalf("failed waiting for witness ack on %d: %v", seq, err)
		}
	}

	local, witness, permanent := stream.Watermarks()
	if local != 10 || witness != 10 {
		t.Fatalf("expected local=10, witness=10, got local=%d, witness=%d, permanent=%d", local, witness, permanent)
	}

	// Verify local files exist before flush
	filesBefore, _ := os.ReadDir(clientDir)
	if len(filesBefore) == 0 {
		t.Fatalf("expected local segment files to exist before permanent flush")
	}

	// Flush to permanent storage
	if err := stream.Flush(ctx); err != nil {
		t.Fatalf("failed to flush to permanent storage: %v", err)
	}

	local, witness, permanent = stream.Watermarks()
	if permanent != 10 {
		t.Fatalf("expected permanent=10 after flush, got %d", permanent)
	}

	// Check that inactive permanent-acked segment files are deleted
	time.Sleep(50 * time.Millisecond)
	filesAfter, _ := os.ReadDir(clientDir)
	if len(filesAfter) > 1 {
		t.Errorf("expected only active segment file to remain after permanent ack, got %d files", len(filesAfter))
	}
}

// 2. Buffer service restarted (ungracefully without flush) with empty data dir:
// Client reconnects, replays un-permanently-acked records, restarted buffer assigns positions >= position_floor,
// and Tail returns all records with no position reused.
func TestBufferServiceRestartWithoutClose(t *testing.T) {
	backend := controller.NewMemoryBackend()
	handle1 := startBufferServer(t, backend, t.TempDir())

	clientDir := t.TempDir()
	streamID := uuid.New()
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()

	stream, err := Open(ctx, clientDir, streamID, handle1.addr)
	if err != nil {
		t.Fatalf("failed to open stream: %v", err)
	}
	defer stream.Close()

	// Append 5 records and flush to permanent storage
	for i := 1; i <= 5; i++ {
		seq, err := stream.Append(ctx, []byte(fmt.Sprintf("record-%d", i)))
		if err != nil {
			t.Fatalf("append %d failed: %v", i, err)
		}
		if err := stream.Wait(ctx, seq, Witness, false); err != nil {
			t.Fatalf("wait witness %d failed: %v", seq, err)
		}
	}
	if err := stream.Flush(ctx); err != nil {
		t.Fatalf("flush failed: %v", err)
	}

	// Append 5 more records (witness only, NOT flushed to permanent storage)
	for i := 6; i <= 10; i++ {
		seq, err := stream.Append(ctx, []byte(fmt.Sprintf("record-%d", i)))
		if err != nil {
			t.Fatalf("append %d failed: %v", i, err)
		}
		if err := stream.Wait(ctx, seq, Witness, false); err != nil {
			t.Fatalf("wait witness %d failed: %v", seq, err)
		}
	}

	// Crash stop buffer service WITHOUT Close() (no graceful flush!)
	handle1.StopWithoutClose()

	// Restart buffer service with empty data dir
	handle2 := startBufferServer(t, backend, t.TempDir())
	defer handle2.StopGraceful()

	// Check that restarted server position allocation starts at >= position_floor of incarnation 1
	if handle2.srv.PositionFloor() <= handle1.srv.PositionFloor() {
		t.Fatalf("expected restarted server position_floor to be > first server position_floor")
	}

	// Connect client to new server
	stream2, err := Open(ctx, clientDir, streamID, handle2.addr)
	if err != nil {
		t.Fatalf("failed to reopen client stream: %v", err)
	}
	defer stream2.Close()

	// Wait for client to replay un-permanently-acked records (6..10) and receive witness acks
	if err := stream2.Wait(ctx, 10, Witness, false); err != nil {
		t.Fatalf("failed waiting for witness ack after restart: %v", err)
	}

	// Tail from position 1
	conn, err := grpc.NewClient(handle2.addr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatalf("dial failed: %v", err)
	}
	defer conn.Close()

	client := pb.NewWalBufferClient(conn)
	tailCtx, tailCancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer tailCancel()

	tailStream, err := client.Tail(tailCtx, &pb.TailRequest{FromPosition: 1})
	if err != nil {
		t.Fatalf("tail failed: %v", err)
	}

	var tailedRecords []*pb.LogRecord
	positionsSeen := make(map[uint64]bool)

	for i := 1; i <= 10; i++ {
		resp, err := tailStream.Recv()
		if err != nil {
			t.Fatalf("tail recv %d failed: %v", i, err)
		}
		if positionsSeen[resp.Record.Position] {
			t.Fatalf("duplicate position %d detected in Tail!", resp.Record.Position)
		}
		positionsSeen[resp.Record.Position] = true
		tailedRecords = append(tailedRecords, resp.Record)
	}

	// Check that all 10 stream records are returned
	streamSeqsSeen := make(map[uint64]bool)
	for _, r := range tailedRecords {
		streamSeqsSeen[r.StreamSeq] = true
	}
	for i := uint64(1); i <= 10; i++ {
		if !streamSeqsSeen[i] {
			t.Errorf("expected stream_seq %d in tailed records, but was missing", i)
		}
	}
}

// 3. Client restarted mid-stream: retained records replayed, duplicates dropped, no gaps in stream_seq.
func TestClientRestartMidStream(t *testing.T) {
	backend := controller.NewMemoryBackend()
	handle := startBufferServer(t, backend, t.TempDir())
	defer handle.StopGraceful()

	clientDir := t.TempDir()
	streamID := uuid.New()
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()

	// Client 1 appends 5 records
	stream1, err := Open(ctx, clientDir, streamID, handle.addr)
	if err != nil {
		t.Fatalf("failed to open stream1: %v", err)
	}
	for i := 1; i <= 5; i++ {
		seq, err := stream1.Append(ctx, []byte(fmt.Sprintf("msg-%d", i)))
		if err != nil {
			t.Fatalf("append %d failed: %v", i, err)
		}
		_ = stream1.Wait(ctx, seq, Witness, false)
	}
	_ = stream1.Close()

	// Client 2 reopens same directory and continues appending records 6..10
	stream2, err := Open(ctx, clientDir, streamID, handle.addr)
	if err != nil {
		t.Fatalf("failed to reopen stream2: %v", err)
	}
	defer stream2.Close()

	local, _, _ := stream2.Watermarks()
	if local != 5 {
		t.Fatalf("expected recovered local watermark 5, got %d", local)
	}

	for i := 6; i <= 10; i++ {
		seq, err := stream2.Append(ctx, []byte(fmt.Sprintf("msg-%d", i)))
		if err != nil {
			t.Fatalf("append %d failed: %v", i, err)
		}
		if err := stream2.Wait(ctx, seq, Witness, false); err != nil {
			t.Fatalf("wait witness %d failed: %v", seq, err)
		}
		if seq != uint64(i) {
			t.Errorf("expected stream_seq %d, got %d (gap detected!)", i, seq)
		}
	}
}

// 4. Two clients interleaved: Tail order is strictly monotonic in position.
func TestTwoClientsInterleaved(t *testing.T) {
	backend := controller.NewMemoryBackend()
	handle := startBufferServer(t, backend, t.TempDir())
	defer handle.StopGraceful()

	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()

	stream1, err := Open(ctx, t.TempDir(), uuid.New(), handle.addr)
	if err != nil {
		t.Fatalf("open stream1 failed: %v", err)
	}
	defer stream1.Close()

	stream2, err := Open(ctx, t.TempDir(), uuid.New(), handle.addr)
	if err != nil {
		t.Fatalf("open stream2 failed: %v", err)
	}
	defer stream2.Close()

	var wg sync.WaitGroup
	wg.Add(2)

	go func() {
		defer wg.Done()
		for i := 1; i <= 10; i++ {
			seq, err := stream1.Append(ctx, []byte(fmt.Sprintf("c1-%d", i)))
			if err == nil {
				_ = stream1.Wait(ctx, seq, Witness, false)
			}
			time.Sleep(1 * time.Millisecond)
		}
	}()

	go func() {
		defer wg.Done()
		for i := 1; i <= 10; i++ {
			seq, err := stream2.Append(ctx, []byte(fmt.Sprintf("c2-%d", i)))
			if err == nil {
				_ = stream2.Wait(ctx, seq, Witness, false)
			}
			time.Sleep(1 * time.Millisecond)
		}
	}()

	wg.Wait()

	// Verify Tail returns 20 records in strictly increasing position
	conn, err := grpc.NewClient(handle.addr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatalf("dial failed: %v", err)
	}
	defer conn.Close()

	client := pb.NewWalBufferClient(conn)
	tailCtx, tailCancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer tailCancel()

	tailStream, err := client.Tail(tailCtx, &pb.TailRequest{FromPosition: 1})
	if err != nil {
		t.Fatalf("tail failed: %v", err)
	}

	var lastPos uint64 = 0
	for i := 1; i <= 20; i++ {
		resp, err := tailStream.Recv()
		if err != nil {
			t.Fatalf("tail recv %d failed: %v", i, err)
		}
		if resp.Record.Position <= lastPos {
			t.Fatalf("position not strictly increasing: prev=%d, curr=%d", lastPos, resp.Record.Position)
		}
		lastPos = resp.Record.Position
	}
}

// 5. Service unreachable: Append keeps succeeding until the retained cap, then blocks; unblocks after reconnect.
func TestServiceUnreachableBackpressure(t *testing.T) {
	backend := controller.NewMemoryBackend()
	handle := startBufferServer(t, backend, t.TempDir())
	handle.StopGraceful() // Terminate service immediately so service is unreachable

	clientDir := t.TempDir()
	streamID := uuid.New()
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()

	// Set small maxRetainedBytes (e.g. 150 bytes, fits ~2 small records)
	stream, err := Open(ctx, clientDir, streamID, handle.addr, WithMaxRetainedBytes(150))
	if err != nil {
		t.Fatalf("open failed: %v", err)
	}
	defer stream.Close()

	// Appends should succeed locally up to cap
	for i := 1; i <= 2; i++ {
		_, err := stream.Append(ctx, []byte("data"))
		if err != nil {
			t.Fatalf("append %d should succeed locally: %v", i, err)
		}
	}

	// Next append should block due to cap
	blockedCtx, blockedCancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
	defer blockedCancel()

	_, err = stream.Append(blockedCtx, []byte("large-payload-that-exceeds-retained-cap-bytes"))
	if err == nil {
		t.Fatalf("expected append to block due to retained cap")
	}
}

// 6. Invalid level in Wait returns error.
func TestInvalidWaitLevel(t *testing.T) {
	clientDir := t.TempDir()
	streamID := uuid.New()
	ctx := t.Context()

	stream, err := Open(ctx, clientDir, streamID, "")
	if err != nil {
		t.Fatalf("open failed: %v", err)
	}
	defer stream.Close()

	seq, err := stream.Append(ctx, []byte("msg"))
	if err != nil {
		t.Fatalf("append failed: %v", err)
	}

	err = stream.Wait(ctx, seq, Level(999), false)
	if err == nil {
		t.Fatalf("expected error on invalid level, got nil")
	}
}

// 7. Wait(Permanent, requestFlush=true) waits for witness first so Flush flushes the newly appended records.
func TestWaitPermanentFlushesAfterWitness(t *testing.T) {
	backend := controller.NewMemoryBackend()
	handle := startBufferServer(t, backend, t.TempDir()) // 10s flush interval
	defer handle.StopGraceful()

	clientDir := t.TempDir()
	streamID := uuid.New()

	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()

	stream, err := Open(ctx, clientDir, streamID, handle.addr)
	if err != nil {
		t.Fatalf("open failed: %v", err)
	}
	defer stream.Close()

	// Append 5 records without waiting for witness individually
	for i := 1; i <= 5; i++ {
		if _, err := stream.Append(ctx, []byte(fmt.Sprintf("msg-%d", i))); err != nil {
			t.Fatalf("append %d failed: %v", i, err)
		}
	}

	// Immediately wait for Permanent with requestFlush=true. Must complete well before 10s periodic flush.
	if err := stream.Wait(ctx, 5, Permanent, true); err != nil {
		t.Fatalf("Wait(Permanent) did not complete before periodic flush: %v", err)
	}

	_, _, permanent := stream.Watermarks()
	if permanent != 5 {
		t.Fatalf("expected permanent watermark 5, got %d", permanent)
	}
}

// 8. Stream.Flush() waits for witness first before issuing Flush RPC.
func TestFlushFlushesAfterWitness(t *testing.T) {
	backend := controller.NewMemoryBackend()
	handle := startBufferServer(t, backend, t.TempDir()) // 10s flush interval
	defer handle.StopGraceful()

	clientDir := t.TempDir()
	streamID := uuid.New()

	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()

	stream, err := Open(ctx, clientDir, streamID, handle.addr)
	if err != nil {
		t.Fatalf("open failed: %v", err)
	}
	defer stream.Close()

	// Append 5 records without waiting for witness individually
	for i := 1; i <= 5; i++ {
		if _, err := stream.Append(ctx, []byte(fmt.Sprintf("msg-%d", i))); err != nil {
			t.Fatalf("append %d failed: %v", i, err)
		}
	}

	// Immediately call Flush. Must complete well before 10s periodic flush.
	if err := stream.Flush(ctx); err != nil {
		t.Fatalf("Flush() did not complete before periodic flush: %v", err)
	}

	_, _, permanent := stream.Watermarks()
	if permanent != 5 {
		t.Fatalf("expected permanent watermark 5, got %d", permanent)
	}
}
