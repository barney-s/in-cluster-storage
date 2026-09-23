# Session: Next Steps on the Streams Work

**Date:** Wednesday, September 23, 2026  
**Topic:** What's the next step on the streams work?

---

## 1. Executive Summary
This document summarizes the current status of streaming implementations across the `in-cluster-storage` subsystems (WAL, ObjectFS, and CAS) and details the concrete next milestones for the streams engineering roadmap. It links our high-level strategic goals with specific files, structs, and functions in the codebase.

---

## 2. WAL Buffer Streaming (`pkg/wal`)

### Current State
*   **Append Stream (`Append`)**: Fully implemented bidirectional gRPC streaming (defined in `proto/wal.proto`). Replicates appended records from `streamImpl` (the client in `pkg/wal/client/client.go`) to the WAL `Server` (the daemon in `pkg/wal/buffer/server.go`).
*   **Tail Stream (`Tail`)**: Server-streaming RPC to read records in sequence. Hardened to read on-demand directly from local SSD segments via `localStore.ReadFrom` or S3/GCS object backends (`ReadSegmentFromBackend`) instead of loading log history into memory.
*   **Reconnect Correctness**: Uses provisional, unflushed positions. Tailing clients re-request from the last flushed position and deduplicate incoming records in-memory using `(stream_id, stream_seq)`, avoiding missing data on daemon restart.

### Next Steps & Technical Design

#### A. Application-Level Flow Control & Backpressure
*   **Problem:** Currently, the client-side backpressure in `Append` is based purely on the local in-memory retained queue size (`retainedBytes > maxRetainedBytes` in `streamImpl.Append`). However, there is no application-level flow control or windowing over the actual gRPC stream. If the daemon's local disk or network becomes slow, or if a `Tail` consumer slows down, memory usage on the buffer daemon or client can grow unchecked.
*   **Implementation Plan:** 
    1. **Protobuf Extension:** Introduce `credits` or `window_size` fields into `AppendResponse` (inside `pb.Ack` or a new message type).
    2. **Client-side Adaptor:** Modify `streamImpl.backgroundReplicationLoop` and `runStreamSession` to track remaining transmission credits. The client must pause sending new `AppendRecord` requests when credits reach zero, blocking in `Append()` until the server receives and acknowledges records, advancing the window.
    3. **Server-side Windowing:** In `Server.Append` (located in `pkg/wal/buffer/server.go`), dynamically calculate available buffer capacity based on the difference between the committed sequence and the read sequence, periodically sending credit updates to the client.

#### B. Connection Multiplexing
*   **Problem:** Currently, the WAL client (`streamImpl`) initiates one bidirectional gRPC connection per logical stream. If a single Kubernetes node has dozens of active streams, this leads to dozens of TCP sockets, TLS handshakes, and active goroutines.
*   **Implementation Plan:**
    1. **Refactor Proto:** Update `proto/wal.proto` to support multi-stream multiplexing. Instead of sending a single `Hello` with one `stream_id` to initialize the bidi-stream, support a `RegisterStreams` message that associates multiple `stream_id` values with the physical connection.
    2. **Payload Tagging:** Carry the 16-byte `stream_id` explicitly inside each `AppendRecord` payload rather than assuming a single pinned stream ID per physical gRPC stream session.
    3. **Connection Pooling:** Implement a connection manager/pool in `pkg/wal/client/client.go` that shares a single gRPC `ClientConn` and multiplexes multiple logical `streamImpl` instances over a single underlying physical bidirectional gRPC stream.

#### C. Stream Compaction & Garbage Collection
*   **Problem:** Currently, local segment files are deleted through `last_position` minus `TailCacheBytes` during flushes, but there is no active deletion or compaction of abandoned streams inside the S3/GCS manifest.
*   **Implementation Plan:**
    1. **Tombstones in Manifest:** Extend the `Manifest` struct (defined in `pkg/wal/buffer/manifest.go`) to record a map of active stream IDs with their last activity timestamp and a list of deleted/tombstoned stream IDs.
    2. **Compaction Worker:** Introduce a background stream retirement worker inside the WAL `Server`.
    3. **TTL Deletion:** Periodically scan the manifest, identify streams that haven't received appends within a configurable TTL (e.g., 7 days), and trigger physical deletion of their corresponding S3/GCS objects followed by removing their entries from the manifest.

---

## 3. ObjectFS CSI Streaming (`pkg/objectfs`)

### Current State
*   **WatchVolume Stream (`WatchVolume`)**: A server-streaming gRPC interface in `proto/objectfs.proto` allowing FUSE node-daemons to subscribe and receive real-time metadata invalidation events broadcast by the central `objectfs-controller` via `EventBroadcaster` in `pkg/objectfs/controller/events.go`.
*   **Local Caching**: The node-daemon intercepts POSIX syscalls via `go-fuse` and serves them from local cache or requests them from the controller.

### Next Steps & Technical Design

#### A. S3/GCS Multipart Upload Streaming for Large Files (>100MB)
*   **Problem:** Multipart upload streaming is currently unimplemented. Writing large files forces either massive local memory buffering or large chunk writes, creating a memory bottleneck on the controller.
*   **Implementation Plan:**
    1. **Extend Backend Interface:** Add multipart upload methods to `Backend` in `pkg/objectstore/objectstore.go`:
       ```go
       InitiateMultipartUpload(ctx context.Context, volumeID, key string) (uploadID string, err error)
       UploadPart(ctx context.Context, volumeID, key, uploadID string, partNumber int32, body io.Reader, length int64) (etag string, err error)
       CompleteMultipartUpload(ctx context.Context, volumeID, key, uploadID string, parts []CompletedPart) (etag string, err error)
       AbortMultipartUpload(ctx context.Context, volumeID, key, uploadID string) error
       ```
    2. **Provider Implementations:** Implement these APIs in `pkg/objectstore/s3storage/s3storage.go` using the AWS SDK S3 client, and in `pkg/objectstore/gcsstorage/gcsstorage.go` using GCS's XML multipart upload or GCP Resumable Upload APIs.
    3. **Streaming Controller Integration:** Modify `pkg/objectfs/controller/volume.go` to stream file blocks exceeding 100MB directly to GCS/S3 concurrently using concurrent `UploadPart` calls, bypassing local memory caching.

#### B. WatchVolume Resiliency and Catchup Protocol
*   **Problem:** If the FUSE node-daemon disconnects from the `WatchVolume` gRPC invalidation stream due to a temporary network partition, it will miss file updates, resulting in local metadata drift and consistency violations. Also, the current `EventBroadcaster` silently drops subscribers whose channel buffers are full.
*   **Implementation Plan:**
    1. **Epoch and Sequence Numbering:** Add an epoch ID (UUID) and a monotonically increasing sequence number to the `WatchVolumeResponse` message in `proto/objectfs.proto`.
    2. **Buffered Event Ring-Buffer:** Modify `EventBroadcaster` in `pkg/objectfs/controller/events.go` to maintain an in-memory ring-buffer of the last $N$ broadcast events per volume.
    3. **Catchup Negotiation:** When a node-daemon reconnects and requests `WatchVolume`, it transmits its last seen sequence number and epoch ID. If the epoch matches and the missed events still exist in the ring-buffer, the controller replays them. If the epoch has changed or the events have fallen out of the ring-buffer, the controller returns a "force full re-sync" signal, prompting the node-daemon to clear and reload its local metadata cache.

---

## 4. CAS Streaming Fallback (`cas`)

### Current State
*   **Zero-Copy SCM_RIGHTS**: The CAS client explicitly avoids streaming bytes over network/Unix sockets. On `GET <sha256>`, the local node CAS daemon (`cas-node-daemon`) passes an open file descriptor (FD) of the cached blob directly to the client container via `SCM_RIGHTS` ancillary control messages.

### Next Steps & Technical Design

#### A. Dual-Path Remote Streaming Fallback
*   **Problem:** SCM_RIGHTS FD passing works beautifully for co-located containers on the same physical Kubernetes node. However, it cannot traverse physical machine boundaries (e.g., if a pod needs to fetch a blob from a CAS cache hosted on a different node or inside a remote service).
*   **Implementation Plan:**
    1. **Client Fallback Logic:** Enhance `Client.RequestBlob` in `cas/pkg/cas/client.go`. If the Unix Domain Socket connection fails (or is determined to be remote/non-co-located, or the server responds with a fallback signal), transition to a network-based streaming fallback path.
    2. **Byte Streaming RPC:** Implement a standard gRPC chunk-based byte stream inside a remote CAS service, or reuse `DownloadBlob(sha256)` (from the `AgentFSController` in `proto/agentfs.proto`) to download the blob chunks in sequence.
    3. **Transparent Interface:** Expose an `io.Reader` or temporary file handle transparently to the calling application so that clients do not need to care whether the blob was received via local SCM_RIGHTS FD-passing or remote gRPC stream chunking.
