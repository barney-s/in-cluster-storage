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

package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"time"

	pb "github.com/gke-labs/in-cluster-storage/pkg/api/wal/v1alpha1"
	"github.com/gke-labs/in-cluster-storage/pkg/wal/client"
	"github.com/google/uuid"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

func main() {
	if len(os.Args) < 2 {
		fmt.Println("Usage: wal-client-test <append|tail> [flags]")
		os.Exit(1)
	}

	cmd := os.Args[1]
	switch cmd {
	case "append":
		runAppend(os.Args[2:])
	case "tail":
		runTail(os.Args[2:])
	default:
		fmt.Printf("Unknown command: %s\n", cmd)
		os.Exit(1)
	}
}

func runAppend(args []string) {
	fs := flag.NewFlagSet("append", flag.ExitOnError)
	dir := fs.String("dir", "/data/wal", "Local WAL directory")
	streamIDStr := fs.String("stream-id", "", "Stream UUID")
	target := fs.String("target", "wal-buffer:50051", "WAL buffer service target")
	count := fs.Int("count", 10, "Number of records to append")
	waitLevelStr := fs.String("wait-level", "witness", "Durability level to wait for (local, witness, permanent)")
	doFlush := fs.Bool("flush", false, "Whether to call Flush to permanent storage at the end")
	holdOpen := fs.Duration("hold-open", 0, "Duration to hold stream open before exiting")
	_ = fs.Parse(args)

	var streamID uuid.UUID
	var err error
	if *streamIDStr != "" {
		streamID, err = uuid.Parse(*streamIDStr)
		if err != nil {
			fmt.Printf("invalid stream-id %q: %v\n", *streamIDStr, err)
			os.Exit(1)
		}
	} else {
		streamID = uuid.New()
	}

	var waitLevel client.Level
	var requestFlush bool
	switch *waitLevelStr {
	case "local":
		waitLevel = client.Local
	case "witness":
		waitLevel = client.Witness
	case "permanent", "s3":
		waitLevel = client.Permanent
		requestFlush = true
	default:
		fmt.Printf("invalid wait-level %q\n", *waitLevelStr)
		os.Exit(1)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	stream, err := client.Open(ctx, *dir, streamID, *target)
	if err != nil {
		fmt.Printf("Failed to open stream: %v\n", err)
		os.Exit(1)
	}
	defer stream.Close()

	for i := 1; i <= *count; i++ {
		payload := []byte(fmt.Sprintf("wal-record-%s-%d", streamID.String(), i))
		seq, err := stream.Append(ctx, payload)
		if err != nil {
			fmt.Printf("Append error on record %d: %v\n", i, err)
			os.Exit(1)
		}

		if err := stream.Wait(ctx, seq, waitLevel, requestFlush); err != nil {
			fmt.Printf("Wait error on record %d: %v\n", i, err)
			os.Exit(1)
		}
	}

	if *doFlush {
		if err := stream.Flush(ctx); err != nil {
			fmt.Printf("Flush error: %v\n", err)
			os.Exit(1)
		}
	}

	if *holdOpen > 0 {
		fmt.Printf("Holding stream open for %v...\n", *holdOpen)
		time.Sleep(*holdOpen)
	}

	local, witness, permanent := stream.Watermarks()
	fmt.Printf("SUCCESS stream_id=%s count=%d local=%d witness=%d permanent=%d\n", streamID.String(), *count, local, witness, permanent)
}

func runTail(args []string) {
	fs := flag.NewFlagSet("tail", flag.ExitOnError)
	target := fs.String("target", "wal-buffer:50051", "WAL buffer service target")
	fromPos := fs.Uint64("from-pos", 1, "Starting position")
	count := fs.Int("count", 10, "Number of records to read")
	_ = fs.Parse(args)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	conn, err := grpc.NewClient(*target, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		fmt.Printf("Failed to dial target %s: %v\n", *target, err)
		os.Exit(1)
	}
	defer conn.Close()

	walClient := pb.NewWalBufferClient(conn)
	tailStream, err := walClient.Tail(ctx, &pb.TailRequest{FromPosition: *fromPos})
	if err != nil {
		fmt.Printf("Tail RPC error: %v\n", err)
		os.Exit(1)
	}

	var lastPos uint64 = 0
	for i := 1; i <= *count; i++ {
		resp, err := tailStream.Recv()
		if err != nil {
			fmt.Printf("Tail recv error on record %d: %v\n", i, err)
			os.Exit(1)
		}
		if resp.Record.Position <= lastPos {
			fmt.Printf("Invalid order: prev=%d, curr=%d\n", lastPos, resp.Record.Position)
			os.Exit(1)
		}
		lastPos = resp.Record.Position
	}

	fmt.Printf("SUCCESS tailed=%d last_position=%d\n", *count, lastPos)
}
