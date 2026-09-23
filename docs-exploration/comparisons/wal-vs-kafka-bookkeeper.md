# Comparison: WAL Buffer Streaming vs. Apache Kafka & Apache BookKeeper

This document analyzes how the **WAL Buffer record streaming subsystem** compares to industry-standard distributed log and ledger engines like **Apache Kafka** and **Apache BookKeeper** in terms of target use cases, data path topology, flow control, and scalability.

---

## 1. Philosophical Target & Use Cases

| Subsystem / System | WAL Buffer (`pkg/wal`) | Apache Kafka | Apache BookKeeper |
| :--- | :--- | :--- | :--- |
| **Primary Goal** | Low-latency cloud staging buffer for Kubernetes CSI volumes. | Distributed event streaming platform & message bus. | High-performance, low-latency replicated ledger storage. |
| **Data Lifespan** | Transient. Log segments are deleted once flushed to GCS/S3. | Durable. Logs are kept based on retention policy (size/time). | Durable. Ledgers are kept until explicitly deleted by clients. |
| **Deployment Model** | Co-located with CSI daemons inside K8s worker clusters. | Dedicated stateful broker clusters with Zookeeper/KRaft. | Bookie storage nodes, typically paired with ZooKeeper. |

### WAL Buffer (`pkg/wal`)
*   **Purpose:** Act as an ephemeral write-ahead log staging area. It intercepts and persists writes locally on NVMe/SSD "witness" segments to guarantee durability, asynchronously flushing those closed segments to S3/GCS.
*   **Lifecycle:** Records are immediately deleted from the local disk when they are flushed to permanent cloud object storage. It is not an arbitrary replay or pub-sub broker.

### Apache Kafka
*   **Purpose:** Serve as a multi-consumer, highly available pub-sub stream. It is designed to retain streams of event data indefinitely or for long durations, allowing arbitrary concurrent reads and historical search.
*   **Lifecycle:** Messages are written to partition logs, replicated across brokers, and expired through time-based or size-based retention policies.

### Apache BookKeeper
*   **Purpose:** Provide replicated, append-only log storage (ledgers). BookKeeper is used as a low-latency log backend for other systems like Apache Pulsar or HDFS NameNodes.
*   **Lifecycle:** Ledgers are appended to and closed, then retained until the orchestrating master service deletes them.

---

## 2. Streaming Data Path & Protocol

```mermaid
graph TD
    subgraph WAL Buffer (Lightweight)
        Client[WAL Client] -- bidi gRPC Append --> Daemon[Local SSD Daemon]
        Daemon -- Async S3/GCS Upload --> ObjectStore[Cloud Object Store]
    end
    subgraph Apache Kafka (Heavyweight)
        Producer[Kafka Producer] -- Custom TCP Binary --> BrokerA[Leader Broker]
        BrokerA -- Sync/Async Replication --> BrokerB[Follower Broker]
    end
```

### WAL Buffer Streaming
*   **Protocol:** Standard bidirectional gRPC streaming (`Append`). The first message initializes the stream with a `Hello` payload, and subsequent record chunks are sent over the same stream.
*   **Watermarks:** Uses a 3-tier watermark model: `localSeq` (local node NVMe disk write), `witnessSeq` (daemon SSD write confirmation), and `s3Seq` (permanent manifest flush acknowledgement).
*   **Tailing:** Consumers tail the merged stream via a server-streaming gRPC `Tail` RPC. If the consumer requests historical entries that have already been flushed and deleted from the local disk, the daemon transparently fetches and streams them from GCS/S3 object storage, merging live in-memory updates on top.

### Apache Kafka
*   **Protocol:** Custom TCP binary protocol. It multiplexes records into batches and partitions to maximize throughput.
*   **Replication:** Brokers replicate partition partitions between leader and follower brokers using in-sync replicas (ISR) and a high watermark.
*   **Tailing:** Consumers fetch records using the `Fetch` protocol, which reads directly from page-caches or disks, utilizing zero-copy `sendfile` system calls to pipe log bytes directly to sockets.

### Apache BookKeeper
*   **Protocol:** Custom Protobuf-based TCP protocol. It uses an "ensemble" of storage nodes (Bookies).
*   **Write Path:** Writes are replicated in parallel to a quorum of bookies (Write Quorum / Ack Quorum), ensuring strict ordering and immediate write-ahead journaling on each bookie.

---

## 3. Flow Control and Connection Multiplexing

### WAL Buffer
*   **Current State:**
    *   **Flow Control:** Purely client-side memory limits (`retainedBytes > maxRetainedBytes`). If the daemon slow-down increases queue size above `maxRetainedBytes`, client-side `Append()` blocks in user space. There is no active gRPC-level flow-control or credit exchange on the wire.
    *   **Multiplexing:** The WAL client opens **one TCP connection per logical stream**, creating significant socket and goroutine overhead when dozens of CSI volumes are active on a node.
*   **Target Roadmap:**
    *   Introduce application-level credits-based windowing over the bidi gRPC streams.
    *   Refactor the protocol to support single-connection multi-stream multiplexing.

### Apache Kafka
*   **Flow Control:** Uses broker-side TCP socket backpressure and configured client quotas (bytes/sec). If a client exceeds its write rate, the broker delays sending responses or pauses reading from the TCP socket.
*   **Multiplexing:** Connects to brokers using a pooled client manager. A single connection per broker is used to multiplex writes and reads for all partitions mapped to that broker.

### Apache BookKeeper
*   **Flow Control:** Employs a pipeline model where clients track outstanding requests and apply client-side throttle limits based on pending add requests.
*   **Multiplexing:** BookKeeper clients reuse connections to Bookies, multiplexing all ledger write-adds over a single netty-based TCP socket channel.

---

## 4. Architectural Summary

WAL Buffer streaming is highly specialized for **ephemeral cloud-edge coordination**. By trading off multi-subscriber pub-sub topologies, partition-sharding, and expensive inter-broker consensus, it achieves a highly efficient, lightweight footprint that runs seamlessly co-located with storage CSI drivers. It leans on cloud-managed GCS/S3 to solve high-availability and durable long-term storage, keeping the in-cluster active state tiny and fast.
