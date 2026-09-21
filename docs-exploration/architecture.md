# Architecture

This document describes how components interact and data flows through the `in-cluster-storage` subsystems.

---

## 1. AgentFS (CSI & OverlayFS)

AgentFS operates via a Stateful Controller and a Node Daemon (CSI driver).

```mermaid
graph TD
    subgraph Host Node
        Pod[Pod Container] -- Mounts --> Target[Merged View /targetPath]
        Target -- OverlayFS --> Upper[upper/ Writable Layer]
        Target -- OverlayFS --> Lower[lower/ Read-Only Layer]
        Lower -- Optional Mount --> EROFS[EROFS Image: snapshot.img]
        Daemon[agentfs-node-daemon] -- Manages --> Lower & Upper & Target
        Daemon -- fanotify interceptor --> Target
    end
    subgraph Control Plane
        Daemon -- gRPC --> Controller[agentfs-controller]
        Controller -- Stores Snapshots --> Storage[(Object Store/Disk)]
    end
```

### Key Lifecycle Flows:
* **Mount (`NodePublishVolume`)**: Node Daemon downloads snapshot metadata. If EROFS is active, it mounts the snapshot image into `lower/` with one syscall. If lazy-loading is active, it sets up `fanotify` on the target directory and intercepts reads to lazily fetch missing files from the controller into `upper/`.
* **Unmount (`NodeUnpublishVolume`)**: Node Daemon scans *only* `upper/` to compute SHA256 hashes of modified or new files, uploads them to the controller, parses whiteouts to track deletions, and commits a delta snapshot.

---

## 2. ObjectFS (Credential-Isolated FUSE)

ObjectFS decouples object storage from node-level worker pods.

```mermaid
sequenceDiagram
    participant Pod as Pod Container
    participant FUSE as Node FUSE Mount
    participant Daemon as objectfs-node-daemon
    participant Controller as objectfs-controller
    participant S3 as GCS / S3 Storage

    Pod->>FUSE: read/write files
    FUSE->>Daemon: System Call (via go-fuse)
    Daemon->>Daemon: Check Local Node Cache
    alt Cache Miss
        Daemon->>Controller: gRPC Read/Write Call
        Controller->>S3: Interact with Cloud Storage
        Controller-->>Daemon: Return file blocks / Signed Redirect URL
    end
    Controller-->>Daemon: WatchVolume (Live Invalidation Stream)
    Daemon->>Daemon: Invalidate Local Cache entries
```

---

## 3. CAS (UDS with SCM_RIGHTS Zero-Copy)

For high-performance content addressable retrieval.

```mermaid
sequenceDiagram
    participant Pod as Container / Client Application
    participant UDS as targetPath/.in-cluster-storage/api
    participant Server as cas-node-daemon (CAS Server)

    Pod->>UDS: Connect & Write "GET <sha256>\n"
    Server->>Server: Look up file in Local Cache (/var/cache/cas)
    alt Cache Miss
        Server->>Server: Pull blob from Controller
    end
    Server->>Server: Open file descriptor (FD) of cached blob
    Server-->>Pod: Send "OK <size>\n" + FD via SCM_RIGHTS (Ancillary OOB data)
    Note over Pod: Read file directly using the received FD (Zero-Copy)
```

---

## 4. WAL Buffer (Segmented Record Streaming & Cloud Flush)

The WAL Buffer subsystem provides high-performance, low-latency write-ahead log buffering, committing to fast local storage before flushing to cloud object storage.

```mermaid
sequenceDiagram
    participant Client as WAL Client
    participant Server as wal-buffer Daemon
    participant S3 as GCS / S3 Storage

    Client->>Server: Append(stream Hello)
    Server-->>Client: AppendResponse(HelloAck)

    Client->>Server: Append(stream AppendRecord)
    Server->>Server: Write to local log segment (Witness Ack)
    Server-->>Client: AppendResponse(Ack - witness_acked_stream_seq)

    Note over Server: Async Flush (Interval/Size) or Flush RPC
    Server->>S3: Upload closed log segments & write updated Manifest
    Server-->>Client: AppendResponse(Ack - s3_acked_stream_seq)
```

### Key Lifecycle Flows:
* **Append Streaming (`Append`)**: Bidirectional gRPC streams allow clients to write sequential records. The server persists them immediately to local, fast SSD segments (`witness` log) for ultra-low latency, and returns local confirmation.
* **Asynchronous Flushing (`Flush`)**: A background worker uploads local log segments to cloud storage and publishes a new manifest, at which point the server returns the permanent `s3_acked` sequence to the stream clients.
* **Streaming Consumers (`Tail`)**: Clients tail the log from a specified position. Unflushed records are provisional; if a client disconnects and reconnects, they resume from the last flushed position and deduplicate redelivered records in-memory using `(stream_id, stream_seq)`.

