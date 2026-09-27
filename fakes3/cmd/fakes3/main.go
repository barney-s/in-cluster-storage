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

// Command fakes3 serves an in-memory, unauthenticated S3-compatible API for
// tests and local development. It is not a durable store: everything is lost
// when the process exits.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/gke-labs/in-cluster-storage/fakes3/pkg/fakes3"
)

func main() {
	listen := flag.String("listen", ":9000", "Address to listen on (use 127.0.0.1:0 for an ephemeral port)")
	buckets := flag.String("buckets", "", "Comma-separated list of buckets to create at startup")
	autoCreate := flag.Bool("auto-create-buckets", true, "Create a bucket implicitly on the first PutObject to it")
	region := flag.String("region", "us-east-1", "Region reported by GetBucketLocation")
	quiet := flag.Bool("quiet", false, "Do not log each request")
	flag.Parse()

	if err := run(*listen, strings.Split(*buckets, ","), *autoCreate, *region, *quiet); err != nil {
		log.Fatalf("fakes3: %v", err)
	}
}

func run(listen string, buckets []string, autoCreate bool, region string, quiet bool) error {
	opts := []fakes3.Option{
		fakes3.WithBuckets(buckets...),
		fakes3.WithAutoCreateBuckets(autoCreate),
		fakes3.WithRegion(region),
	}
	if !quiet {
		opts = append(opts, fakes3.WithLogger(log.Printf))
	}
	handler := fakes3.New(opts...)

	ln, err := net.Listen("tcp", listen)
	if err != nil {
		return fmt.Errorf("listen on %s: %w", listen, err)
	}

	// The address line goes to stdout so that callers using an ephemeral port
	// (--listen=127.0.0.1:0) can discover it; everything else goes to stderr.
	fmt.Printf("fakes3 listening on http://%s\n", ln.Addr())
	log.Printf("fakes3 listening on http://%s (buckets=%q, auto-create=%v)", ln.Addr(), buckets, autoCreate)

	server := &http.Server{
		Handler:           handler,
		ReadHeaderTimeout: 30 * time.Second,
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	errCh := make(chan error, 1)
	go func() { errCh <- server.Serve(ln) }()

	select {
	case err := <-errCh:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	case <-ctx.Done():
	}

	log.Printf("fakes3 shutting down")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return server.Shutdown(shutdownCtx)
}
