# Triển khai BoltQ trên Kubernetes

Apply theo thứ tự số — `00` định nghĩa Secret và ServiceAccount mà các file sau
tham chiếu.

```bash
# 1. Sinh secret thật. KHÔNG apply 00-base.yaml với giá trị CHANGE_ME.
kubectl create namespace boltq
kubectl -n boltq create secret generic boltq-secrets \
  --from-literal=api-key=$(openssl rand -hex 32) \
  --from-literal=replication-secret=$(openssl rand -hex 32)

# 2. Phần còn lại của 00-base (Namespace/Secret đã tồn tại sẽ báo unchanged).
kubectl apply -f 00-base.yaml

# 3. Control plane trước, đợi quorum, rồi mới tới data tier.
kubectl apply -f 10-controllers.yaml
kubectl -n boltq rollout status statefulset/boltq-controllers

kubectl apply -f 20-data.yaml
kubectl apply -f 30-networkpolicy.yaml
```

## Ba tầng

| File | Vai trò | Scale |
|---|---|---|
| `10-controllers` | Raft voter, giữ metadata + queue log | **Cố định 3 hoặc 5** |
| `20-data` | Stream log, replication, gateway WS | Scale theo tải, join dạng non-voter |
| `30-networkpolicy` | Chặn 9100/9200 khỏi mọi thứ không phải BoltQ | — |

Đừng tăng số voter để "HA hơn". 3 voter chịu được 1 lỗi, 5 voter chịu được 2;
thêm nữa chỉ làm mỗi lần commit metadata chậm đi mà không tăng khả năng chịu lỗi.

## Cổng

| Cổng | Dùng cho | Ai được truy cập |
|---|---|---|
| 9300 | Gateway WebSocket | **Internet** (qua Service `boltq-gateway`) |
| 9090 | Admin HTTP, `/cluster/join`, `/cluster/leave` | Chỉ trong cluster |
| 9091 | Queue TCP | Chỉ trong cluster |
| 9100 | Raft | Chỉ pod BoltQ |
| 9200 | Replication — **đọc được toàn bộ log** | Chỉ pod BoltQ |

Cổng 9200 nguy hiểm nhất: bất kỳ ai bắt tay thành công đều đọc được toàn bộ lịch
sử tin nhắn. NetworkPolicy là lớp bảo vệ thật; secret chỉ là lớp thứ hai.

## Probe

- `/livez` — luôn 200 khi tiến trình còn sống, **không** đọc trạng thái cluster.
  Mất quorum không phải là treo; nếu liveness fail lúc đó, K8s sẽ restart đúng
  những node mà cluster cần đứng yên để bầu leader.
- `/readyz` — 503 khi chưa biết leader. Pod bị rút khỏi endpoint nhưng không bị
  giết.

## Yêu cầu hạ tầng

- **≥3 K8s node** — anti-affinity của controller là `required`. Ít hơn thì pod
  thứ ba ở trạng thái Pending, và đó là hành vi đúng: 2 voter trên 1 máy tệ hơn
  chạy 1 node.
- **CNI có enforce NetworkPolicy** (Calico, Cilium, GKE Dataplane V2). Không có
  thì file `30` apply sạch nhưng không bảo vệ gì.
- **StorageClass SSD** — commit Raft là fsync-bound.

## Chưa có

Replication của message plane đang `BOLTQ_REPLICATION_ENABLED=false`. Role
leader/follower hiện vẫn là config tĩnh, mà một StatefulSet không thể gán cho
pod-0 "leader" và phần còn lại "follower" nếu không muốn hardcode một quyết định
leadership rồi không bao giờ failover được. Xem mục 1 trong
[architecture-ha.md](../../docs/architecture-ha.md) — cần nối heartbeat để
controller cấp phát role.
