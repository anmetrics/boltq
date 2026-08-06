# Tích hợp BoltQ vào hệ thống đang chạy ở quy mô lớn

Tài liệu này viết cho tình huống: hệ thống đã có sẵn user, đã có auth, đã có DB
quan hệ xã hội, và cần đưa BoltQ vào làm tầng vận chuyển tin nhắn. Nó **không**
phải hướng dẫn dựng hệ thống mới.

Nguyên tắc xuyên suốt: **BoltQ không sở hữu user, không sở hữu quan hệ xã hội,
không sở hữu quyền.** Nó sở hữu *đường đi của tin nhắn*. Mọi thất bại tích hợp
mà tôi từng thấy đều bắt đầu từ việc vi phạm câu này — nhân bản social graph vào
broker rồi có hai nguồn sự thật cho cùng một câu hỏi "ai được đọc cái gì".

---

## 1. Ba đường nối, đã có sẵn trong code

BoltQ được thiết kế với đúng ba chỗ chọc ra ngoài. Không có chỗ thứ tư, và đó là
điều khiến nó tích hợp được vào hệ thống đã tồn tại.

### 1.1 Quan hệ xã hội → `membership`

[internal/membership/](../internal/membership/) hỏi ứng dụng, không tự lưu:

```
GET <base>/is-member?tenant=&user=&group=   -> {"member": true}
GET <base>/members?tenant=&group=           -> {"members": ["u1","u2"]}
```

Hai đường tách nhau **vì chi phí khác nhau hàng bậc**: `is-member` là point
lookup chạy trên *mọi* message; `members` là scan chỉ chạy khi fan-out. Gộp lại
là tự bắn vào chân.

Hội thoại 1-1 (`direct:userA:userB`) resolve thành viên từ chính conversation ID
— **không gọi ra ngoài**. Với hầu hết hệ thống chat, đây là 80–90% lưu lượng.
Nghĩa là membership endpoint của bạn chỉ hứng phần group.

> **Bắt buộc ở quy mô của bạn**: endpoint này nằm trên đường đi nóng. Nó cần
> circuit breaker và negative caching. Hiện đã có cache TTL 30s
> (`MembershipCacheTTL`) — chưa đủ. Một sự cố ở DB của app sẽ kéo sập toàn bộ
> chat nếu không có breaker.

### 1.2 Danh tính → `identity`

[internal/identity/](../internal/identity/) verify token đã ký. Bạn **giữ nguyên**
hệ auth hiện tại; chỉ cần phát thêm một token ngắn hạn cho kết nối WebSocket, ký
bằng khoá chia sẻ với BoltQ (`BOLTQ_IDENTITY_KEY`, có key ID để xoay khoá).

BoltQ không bao giờ thấy mật khẩu, không giữ session của bạn, không cần biết
user tồn tại hay không.

### 1.3 Thông báo đẩy → `outbox`

[internal/outbox/](../internal/outbox/) gọi webhook khi người nhận offline, có
`GraceDelay` để không bắn notification cho người vừa đọc trên thiết bị khác.
Nối vào hệ push hiện tại của bạn (APNs/FCM) — BoltQ không cố thay nó.

---

## 2. Kiến trúc cell

Ở cỡ tỉ user, câu hỏi đúng không phải "làm sao một cluster chịu được", mà là
"chia thành bao nhiêu cluster độc lập". Đây là cách WhatsApp, Discord, Slack đều
làm, và cũng là điều duy nhất thực sự scale.

```
                      ┌────────────────────────┐
   client ────────▶   │   Cell Router          │  ← tra bảng, không giữ state
                      │   user_id → cell_id    │
                      └───────────┬────────────┘
              ┌───────────────────┼───────────────────┐
              ▼                   ▼                   ▼
        ┌──────────┐        ┌──────────┐        ┌──────────┐
        │  Cell 1  │        │  Cell 2  │  ...   │  Cell N  │
        │ 3 ctrl   │        │ 3 ctrl   │        │ 3 ctrl   │
        │ 50 data  │        │ 50 data  │        │ 50 data  │
        └──────────┘        └──────────┘        └──────────┘
         độc lập hoàn toàn — không chia sẻ Raft, không chia sẻ log
```

**Sizing** (điều chỉnh theo số thật của bạn):

| | |
|---|---|
| 1 tỉ user đăng ký, ~10% online | ~100M socket đồng thời |
| 150k socket/node | ~700 node phục vụ kết nối |
| 1 cell = 50–100 data node + 3 controller | **10–20 cell** |

Điểm mấu chốt: **mỗi cell chỉ phải giải bài toán 50–100 node**. Đó đúng là cỡ mà
Raft và control plane trong `internal/cluster` xử lý tốt. Cell thứ 20 không khó
hơn cell thứ 2.

### Chọn khoá phân cell

Phân theo **conversation**, không phải theo user. Một cuộc hội thoại phải nằm
trọn trong một cell, nếu không mọi tin nhắn thành giao dịch phân tán hai pha.

```
cell_id = hash(conversation_id) % num_cells
```

Với hội thoại 1-1, conversation_id đã chứa cả hai user ID nên nó ổn định. Với
group, group_id là khoá.

**Đừng dùng modulo trần** cho production — thêm cell sẽ xáo trộn toàn bộ. Dùng
consistent hashing hoặc, tốt hơn ở quy mô này, **một directory service**:
`conversation_id → cell_id` lưu trong DB có sẵn của bạn. Chậm hơn vài trăm
microsecond, đổi lại việc thêm cell trở thành chuyện thường ngày thay vì sự
kiện.

---

## 3. Lộ trình chuyển đổi

Đây là phần quyết định thành bại, và cũng là phần không được phép ứng biến.

**Giai đoạn 0 — một cell, traffic nội bộ.** Dựng một cell theo
[deploy/kubernetes/](../deploy/kubernetes/). Chỉ nhân viên dùng. Mục tiêu không
phải hiệu năng mà là *phát hiện điều bạn hiểu sai về hệ thống của chính mình*.

**Giai đoạn 1 — shadow write.** Hệ thống hiện tại vẫn phục vụ 100% người dùng.
Mỗi tin nhắn được ghi thêm vào BoltQ, kết quả bị bỏ đi. So sánh: thứ tự có giữ
không, độ trễ ra sao, fan-out có khớp không. Chạy tối thiểu hai tuần, phải qua
một đợt cao điểm thật.

**Giai đoạn 2 — đọc song song, một cell.** Chọn 1 cell (≈5% user). Client đọc từ
BoltQ, vẫn giữ đường cũ làm fallback. Đây là lúc phát hiện vấn đề về cursor và
resume — thứ shadow write không bao giờ lộ ra.

**Giai đoạn 3 — cắt từng cell.** Mỗi cell cắt độc lập, cách nhau đủ để quan sát.
Rollback là *đổi một dòng trong directory*, không phải deploy.

**Không bao giờ**: cắt toàn bộ cùng lúc; cắt trong mùa cao điểm; cắt khi chưa có
đường rollback đã diễn tập.

---

## 4. So với Kafka và RabbitMQ

Để bạn biết mình đang chọn cái gì, và khi nào **không** nên chọn nó.

| | BoltQ | Kafka | RabbitMQ |
|---|---|---|---|
| Mô hình | Log phân mảnh + chat semantics | Log phân mảnh | Queue broker |
| Control plane | Raft (`internal/cluster`) | KRaft | Mnesia/Khepri |
| Fencing | Leader epoch + truncation | Leader epoch + truncation | — |
| WebSocket/presence | **Có sẵn** | Không | Không |
| Fan-out inbox | **Có sẵn** | Tự xây | Tự xây |
| Hệ sinh thái | Gần như không | Rất lớn | Lớn |
| Đã kiểm chứng ở tỉ user | **Chưa** | Rồi | Rồi |

Điểm mạnh thật của BoltQ là hai dòng in đậm ở giữa: presence, resume, fan-out
inbox, dedup, cursor — những thứ mà dùng Kafka bạn phải tự viết, và viết đúng
thì mất nhiều quý.

Điểm yếu thật là dòng cuối. **Hãy đối xử với nó đúng như vậy** — đó là lý do
mục 3 tồn tại và không nên rút gọn.

Nếu bài toán của bạn là event streaming đa mục đích cho cả tổ chức, dùng Kafka.
Nếu là task queue với routing phức tạp, dùng RabbitMQ. BoltQ đáng chọn khi bài
toán là **chat/realtime messaging** và bạn muốn tránh viết lại tầng presence +
fan-out lần thứ n.

---

## 5. Trạng thái thật của code hôm nay

Không tô hồng.

**Chạy được, có test, đã verify bằng container thật:**
- Control plane Raft: đăng ký node, heartbeat, fencing tự động, bầu leader
  partition, epoch tăng đơn điệu, preferred-leader rebalance
- Log phân mảnh, epoch + truncation protocol, replication có quorum ack
- Fan-out, dedup, cursor, presence (trong một node), gateway WebSocket có resume
- Deploy K8s ba tầng, probe tách live/ready

- **Rebalance planner** ([internal/cluster/planner.go](../internal/cluster/planner.go)):
  đặt replica rack-aware, và di chuyển replica khi node vào/ra. Giao thức di
  chuyển là **thêm → chờ vào ISR → xoá**, không bao giờ ngược lại: một move bị
  treo để partition ở trạng thái *thừa* bản sao, an toàn tuyệt đối. Bật bằng
  `BOLTQ_REBALANCE=true` — mặc định tắt, vì mỗi move copy nguyên một partition
  qua mạng và đó phải là quyết định của người vận hành.

- **Guard leadership + routing ghi**
  ([forwarder.go](../internal/streamctl/forwarder.go)): node giữ replica nhưng
  không lead thì **từ chối** ghi cục bộ (`ErrNotPartitionLeader`) và chuyển
  write sang node lead. Không dùng redirect vì một socket chat subscribe hàng
  chục hội thoại nằm ở nhiều leader khác nhau — câu hỏi "client này nên nối tới
  node nào" không có câu trả lời. Chỉ **write** đi xa; đọc phục vụ tại chỗ vì
  replica đã có dữ liệu.

**Chưa có — và đây là thứ chặn quy mô:**
1. **Presence liên node.** Đang in-memory từng node. Ở 10M socket, replicate qua
   Raft là sai — cần gossip hoặc shard theo user ID.
2. **Metric message plane ra Prometheus.** Gateway đã đếm `Sessions`,
   `SlowClientDrops`; forwarder đếm `Forwarded`/`Failed`/`NoLeader` — chưa
   export cái nào.
3. **Cursor store.** File-backed. Tỉ user × chục hội thoại = hàng tỉ cursor →
   phải chuyển sang compacted topic.

Mục 1–3 phải xong trước Giai đoạn 3.

---

## 5b. Số đo thật

Đo trên máy 4 core, append 256 byte:

| | ns/op | Ghi chú |
|---|---|---|
| 1 partition, song song | 6488 | Bị khoá partition chặn |
| 8 partition | 5105 | |
| 32 partition | 1558 | **Phân mảnh có tác dụng: nhanh 4×** |
| fsync mỗi append | **25.023.329** | ~40 write/giây/node |
| không fsync | 2206 | |

Dòng áp chót là lý do manifest đặt `SYNC_ON_APPEND=false` với `min_in_sync=2`.
fsync mỗi append chậm hơn **11.000 lần** — không phần cứng nào cứu được. Độ bền
đến từ replication: record được ack nằm trong page cache của hai máy, sống sót
qua crash tiến trình và mất một máy. fsync chỉ thêm khả năng sống sót khi **cả
hai** replica mất điện cùng lúc — đúng cái mà hệ replicated đã làm cho khó xảy
ra, đổi bằng cái giá khiến hệ thống không dùng được. Kafka đánh đổi y hệt.

Đây vẫn **không** phải throughput đầu-cuối: không qua mạng, không qua gateway,
không fan-out. Đừng trích nó như năng lực hệ thống.

## 6. Hành vi khi quá tải

Đã kiểm tra trong code, không phải suy đoán:

- **Client chậm bị ngắt, không lan.** [gateway.go:218](../internal/gateway/gateway.go#L218)
  — hàng đợi đầy thì đóng kết nối. Client reconnect, resume từ cursor,
  **không mất tin nhắn**. Một điện thoại sóng yếu không thể tạo backpressure lên
  server.
- **Signal ephemeral bị bỏ im lặng**, có đếm. Đúng — "đang gõ…" đến muộn thì vô
  nghĩa.
- **Partition không có ISR sống → offline**, chứ không bầu replica lạc hậu. Ghi
  thất bại nhanh thay vì mất dữ liệu âm thầm. Đổi hành vi này bằng
  `AllowUncleanElection`, và chấp nhận mất record đã ack.

Không có cơ chế tự chuyển tải đi khi nghẽn — xem mục 5.1.
