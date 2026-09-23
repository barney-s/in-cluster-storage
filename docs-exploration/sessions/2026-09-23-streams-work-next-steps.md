# Session: Next Steps on the Streams Work

**Date:** Wednesday, September 23, 2026  
**Topic:** What's the next step on the streams work?

---

## 1. Executive Summary
This document summarizes the current status of streaming implementations across the `in-cluster-storage` subsystems (WAL, ObjectFS, and CAS) and details the concrete next milestones for the streams engineering roadmap.

---

## 2. WAL Buffer Streaming (`pkg/wal`)

### Current State
*   **Append Stream (`Append`)**: Fully implemented bidirectional gRPC streaming (defined in `proto/wal.proto`). Replicates appended records from `streamImpl` (the client) to `Server` (the daemon).
*   **Tail Stream (`Tail`)**: Server-streaming RPC to read records in sequence. Hardened in PR #53 and PR #54 to read on-demand directly from local SSD segments or S3/GCS object backends instead of loading log history into memory.
*   **Reconnect Correctness**: Uses provisional, unflushed positions. Tailing clients re-request from the last flushed position and deduplicate incoming records in-memory using `(stream_id, stream_seq)`, avoiding missing data on daemon restart.

### Next Steps & Technical Design

#### A. Application-Level Flow Control & Backpressure
*   **Problem:** Currently, the client-side backpressure in `Append` is based purely on the in-memory retained queue size (`retainedBytes > maxRetainedBytes`). However, there is no application-level flow control or windowing over the actual gRPC stream. If the daemon's local disk or network becomes slow, or if a `Tail` consumer slows down, memory usage on the buffer daemon or client can grow unchecked.
*   **Implementation:** 
    1. Implement gRPC-level flow control using `grpc.MaxSendMsgSize` and custom chunking.
    2. Add an application-level credits-based windowing protocol inside the `Append` and `Tail` message payloads, letting clients know when the server is ready for more records.

#### B. Connection Multiplexing
*   **Problem:** Currently, the WAL client initiates one bidirectional gRPC connection per logical stream. If a single Kubernetes node has dozens of active streams, this leads to dozens of TCP sockets, TLS handshakes, and active goroutines.
*   **Implementation:**
    1. Refactor `proto/wal.proto` and `pkg/wal/client/client.go` to support a single, multiplexed gRPC connection.
    2. Multiplex several logical `stream_id` streams over a single physical gRPC bidi-stream connection. The `Hello` request can register multiple streams, and `AppendRecord` payloads will carry the `stream_id` directly rather than pinning it per gRPC call.

#### C. Stream Compaction & Garbage Collection
*   **Problem:** Currently, local segment files are deleted through `last_position` minus `TailCacheBytes` during flushes, but there is no active deletion or compaction of abandoned streams inside the S3/GCS manifest.
*   **Implementation:**
    1. Introduce a stream retirement worker.
    2. Add a tombstone mechanism to the `Manifest` structure (in `pkg/wal/buffer/manifest.go`) to garbage collect and remove old streams that haven't received appends within a configurable TTL (e.g. 7 days).

---

## 3. ObjectFS CSI Streaming (`pkg/objectfs`)

### Current State
*   **WatchVolume Stream (`WatchVolume`)**: A server-streaming gRPC interface in `proto/objectfs.proto` allowing FUSE node-daemons to subscribe and receive real-time metadata invalidation events broadcast by the central `objectfs-controller`.
*   **Local Caching**: The node-daemon intercepts POSIX syscalls via `go-fuse` and serves them from local cache or requests them from the controller.

### Next Steps & Technical Design

#### A. S3/GCS Multipart Upload Streaming for Large Files (>100MB)
*   **Problem:** As highlighted in the `docs/research/objectfs.md` roadmap, multipart upload streaming is currently un-checked (unimplemented). Writing large files forces either massive local memory buffering or large chunk writes, creating a memory bottleneck on the controller.
*   **Implementation:**
    1. Extend the `objectstore.Backend` interface in `pkg/objectstore/objectstore.go` to support chunked multipart upload session initialization, upload part, and complete/abort parts.
    2. Implement these APIs in `s3storage` and `gcsstorage` backends.
    3. Update `pkg/objectfs/controller` to stream file chunks to GCS/S3 concurrently, bypassing local memory caching for files exceeding the 100MB threshold.

#### B. WatchVolume Resiliency and Catchup Protocol
*   **Problem:** If the FUSE node-daemon disconnects from the `WatchVolume` gRPC invalidation stream due to a temporary network partition, it will miss file updates, resulting in local metadata drift and consistency violations.
*   **Implementation:**
    1. Add an epoch ID and a monotonically increasing sequence number to invalidation events in `proto/objectfs.proto`.
    2. When reconnecting to `WatchVolume`, the node-daemon must transmit its last seen sequence/epoch.
    3. The controller can then replay missed events from an in-memory ring-buffer, or reply with a "force full re-sync" signal if the connection was lost for too long.

---

## 4. CAS Streaming Fallback (`cas`)

### Current State
*   **Zero-Copy SCM_RIGHTS**: The CAS client explicitly avoids streaming bytes over network/Unix sockets. On `GET <sha256>`, the local node CAS daemon passes an open file descriptor (FD) of the cached blob directly to the client container via `SCM_RIGHTS` ancillary control messages.

### Next Steps & Technical Design

#### A. Dual-Path Remote Streaming Fallback
*   **Problem:** SCM_RIGHTS FD passing works beautifully for co-located containers on the same physical Kubernetes node. However, it cannot traverse physical machine boundaries (e.g., if a pod needs to fetch a blob from a CAS cache hosted on a different node or inside a remote service).
*   **Implementation:**
    1. Add a fallback streaming path to the CAS client protocol.
    2. If the CAS socket connection fails or is determined to be remote (non-host-path UDS), the client falls back to requesting a standard gRPC chunk-based byte stream from the CAS daemon.
