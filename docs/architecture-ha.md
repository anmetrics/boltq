# BoltQ — Kiến trúc HA cho quy mô trăm triệu user

Tài liệu này mô tả kiến trúc đích và trạng thái hiện tại. Phần "đã có" là code
chạy được trong repo; phần "còn thiếu" là việc chưa làm, ghi rõ để không ai đọc
nhầm bản thiết kế thành bản đã triển khai.

## 1. Nguyên tắc nền

**Metadata đi qua consensus, data đi qua log replication.**

Đây là quyết định kiến trúc quan trọng nhất, và là lý do Kafka bỏ ZooKeeper để
chuyển sang KRaft. Nếu đẩy từng message qua Raft, mọi write phải trả giá một
quorum fsync và cả cluster bị chặn ở throughput của consensus — vài chục nghìn
op/s là trần. Tách ra:

| Loại | Đường đi | Thông lượng |
|---|---|---|
| Ai lead partition nào, ISR, epoch | Raft (`internal/cluster`) | ~hàng trăm thay đổi/giây, đủ dư |
| Record thật | Leader→follower pipeline (`internal/replication`) | sequential append, giới hạn bởi đĩa/mạng |

Control plane chỉ ghi khi *có sự kiện* — node chết, partition chuyển leader,
ISR co giãn. Ở trạng thái ổn định nó im lặng hoàn toàn (được đảm bảo bằng
`TestControllerIsIdempotent`).

## 2. Sizing: trăm triệu user nghĩa là gì

Giả định làm việc, chỉnh theo số liệu thật khi có:

- 100M user đăng ký, ~10% online đồng thời → **10M WebSocket concurrent**
- Mỗi gateway node giữ ~100k–200k socket (Go, epoll, ~10–20KB/conn) →
  **50–100 gateway node**
- 5M msg/s đỉnh, mỗi partition xử lý ~10–20k msg/s → **~300–500 partition**
  cho conversation topic, cộng inbox topic phân mảnh theo user
- Replication factor 3, min_in_sync 2 → chịu được mất 1 node không mất dữ liệu

Con số quyết định thiết kế: **số partition (hàng nghìn) lớn hơn số node (hàng
trăm) một bậc**. Đó chính là lý do leadership phải ở mức *partition*, không phải
mức *node* — leadership mức node biến mỗi lần failover thành việc dịch chuyển
toàn bộ tải của một máy sang một máy khác, và cluster dao động thay vì ổn định.

## 3. Ba tầng

```
                     ┌──────────────────────────────────┐
   client ──TLS──▶   │  Gateway tier (stateless)        │  50–100 node
                     │  WebSocket, auth, rate limit     │
                     └───────────────┬──────────────────┘
                                     │ route theo metadata cache
                     ┌───────────────▼──────────────────┐
                     │  Data tier (stateful)            │  hàng trăm node
                     │  stream.Log, partition leader/   │
                     │  follower, replication pipeline  │
                     └───────────────┬──────────────────┘
                                     │ đọc metadata, gửi ISR
                     ┌───────────────▼──────────────────┐
                     │  Control plane (Raft, 3–5 node)  │
                     │  ai lead gì, ISR, epoch, fencing │
                     └──────────────────────────────────┘
```

**Control plane tách riêng 3–5 node** khi cluster vượt ~20 node. Trước ngưỡng đó
chạy chung với data node là hợp lý; sau đó thì không, vì mọi data node là voter
sẽ khiến mỗi lần commit metadata phải chờ quorum của hàng trăm máy. Data node
tham gia dưới dạng *non-voter* (`JoinNonVoter` đã có sẵn): nhận metadata, không
bỏ phiếu.

## 4. Vòng đời một failover

Đây là chuỗi phải đúng, và là phần đã được implement + test:

1. Node `n1` ngừng heartbeat.
2. Controller (= Raft leader) hết `SessionTimeout` → apply `CmdMetaFenceBroker`.
   Fence *đồng thời* loại `n1` khỏi mọi ISR — một node không nghe thấy thì không
   thể tính vào durability.
3. Controller quét từng partition `n1` đang lead, chọn leader mới trong
   `ISR ∩ live`, bump epoch, apply `CmdMetaAssignLeader`.
4. Node được chọn nhận metadata event → `BecomeLeaderFor(topic, pid, epoch)`.
   Epoch đến từ consensus, không tự chế — đây là điều khiến hai node không thể
   cùng tin mình lead một partition ở cùng một epoch.
5. Follower kết nối lại, gửi epoch cuối của nó; leader trả về epoch đó *kết thúc
   ở seq nào*; follower truncate phần đuôi thuộc về term đã bị bỏ.
   (`internal/stream/epoch.go`, đã có từ trước.)
6. `n1` sống lại → unfence → bắt kịp → vào lại ISR → preferred-leader rebalance
   kéo leadership về chỗ cũ để tải không dồn cục.

Nếu ISR rỗng, partition được đánh dấu **offline** (leader rỗng) thay vì bầu một
replica lạc hậu. Write fail nhanh với "partition offline". Muốn đổi sang ưu tiên
availability thì bật `AllowUncleanElection` — và chấp nhận mất record đã ack.

## 5. Trạng thái hiện tại

### Đã có (code chạy, có test)

- `internal/cluster/metadata.go` — metadata store replicated: broker registry,
  topic, partition assignment, ISR, epoch, snapshot/restore, event stream.
- `internal/cluster/controller.go` — controller chạy trên Raft leader: phát hiện
  node chết, fence, bầu leader partition, rebalance về preferred leader,
  startup grace chống fence hàng loạt khi controller vừa đổi.
- `internal/cluster/fsm.go` — hai plane dùng chung một Raft log (bắt buộc: hai
  FSM = hai thứ tự khác nhau cho cùng chuỗi lệnh).
- `internal/streamctl/reconciler.go` — phía node: metadata → hành động thật
  (promote partition, mở/đóng session replication theo từng leader node).
- `internal/stream/log.go` — `BecomeLeaderFor` promote từng partition.
- `internal/api/http_health.go` — tách `/livez` (không bao giờ đọc trạng thái
  cluster) khỏi `/readyz` (503 khi chưa biết leader).
- `deploy/kubernetes/` — manifests ba tầng: controller (voter cố định), data
  (non-voter + gateway), NetworkPolicy. Xem
  [deploy/kubernetes/README.md](../deploy/kubernetes/README.md).

### Còn thiếu (theo thứ tự ưu tiên)

1. **Heartbeat transport + đăng ký node lúc boot.** Controller đã có
   `Heartbeat(nodeID)`; chưa có endpoint HTTP để node gọi tới Raft leader, và
   `buildMessaging` chưa apply `CmdMetaRegisterBroker`. Không có bước này thì
   control plane đúng nhưng đứng yên.
2. **Gateway redirect.** Client subscribe partition mà node không lead → phải
   proxy hoặc redirect tới node lead, giống `NotLeaderError` bên queue plane.
3. **Presence cross-node.** Hiện `presence.Registry` là in-memory từng node; đã
   có sẵn field `NodeID`/`Local` nhưng không có cơ chế nào ghi session của node
   khác vào. Với 10M socket, replicate qua Raft là sai (quá nhiều ghi) — dùng
   gossip (`hashicorp/memberlist`) hoặc shard presence theo user ID.
4. **Placement tự động khi tạo topic.** `applyCreateTopic` nhận placement từ
   caller; chưa có bộ chọn rack-aware.
5. **`internal/queuelog` chưa được nối vào server** — AMQP semantics trên stream
   log đã viết xong nhưng chưa ai gọi.

## 6. Những chỗ sẽ vỡ trước ở quy mô lớn

Ghi ra để không phải phát hiện lúc 3 giờ sáng:

- **Fan-out on write cho group lớn.** `FanoutOnWriteLimit` đã có, nhưng cần số
  liệu thật: một group 100k thành viên mà fan-out on write sẽ tạo 100k inbox
  pointer cho một message.
- **Cursor store.** Mỗi user × mỗi conversation là một cursor. 100M user × chục
  conversation = hàng tỷ cursor; file-backed store hiện tại không chịu nổi, cần
  chuyển sang compacted topic (chính stream log tự lưu cursor của mình).
- **Membership endpoint** ([internal/membership/](../internal/membership/)) nằm
  trên đường đi của *mọi* message. Cache 30s hiện tại là đúng hướng nhưng cần
  negative caching và circuit breaker, nếu không một sự cố ở app DB sẽ kéo sập
  toàn bộ chat.
- **Metadata cache của client.** Mỗi assignment có `Version`; client phải gửi
  kèm để phát hiện mình đang cầm bản đồ cũ. Chưa nối vào protocol.

## 7. Vận hành

- **min_in_sync = 2, RF = 3.** RF=2 nghĩa là mất một node thì partition không
  còn dự phòng; RF=3 là mức thấp nhất cho phép mất một node mà vẫn giữ nguyên
  đảm bảo.
- **SessionTimeout 15s** là điểm cân bằng: thấp hơn thì một GC pause gây failover
  vô ích, cao hơn thì write vào partition của node chết treo đúng bấy nhiêu giây.
- **Không bao giờ bật `AllowUncleanElection` mặc định.** Nó đánh đổi record đã
  ack lấy uptime, và không ai phát hiện ra cho tới khi user hỏi tin nhắn đâu.
- **TLS chỉ ở gateway.** Listener replication (`internal/replication`) đọc được
  toàn bộ log — tuyệt đối không expose ra internet; secret rỗng chỉ chấp nhận
  được trên mạng nội bộ tin cậy.
