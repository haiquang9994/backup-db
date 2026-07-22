# Deploy: build local, upload lên server

Script `deploy.sh` build Docker image `backupdb:latest` ngay trên máy bạn, rồi đóng gói và đẩy sang server bằng `docker save` → `scp` → `docker load`, không cần image registry (Docker Hub, GHCR...). Dùng khi bạn không muốn (hoặc không thể) build trực tiếp trên server.

## Cần chuẩn bị trước

- Máy local: đã cài Docker, có quyền build (`docker build`).
- SSH vào server bằng key, không cần nhập mật khẩu (script gọi `ssh`/`scp` không tương tác) — kiểm tra bằng `ssh <DEPLOY_HOST> echo ok`.
- Server đã có sẵn checkout của repo này tại `DEPLOY_PATH`, kèm `.env` và `google/credentials.json` đã cấu hình, network `dbnet` đã tạo — **script này không đụng tới các file đó**, chỉ thay image rồi khởi động lại.
- Nếu `docker-compose.yml` hoặc các file khác vừa sửa trên máy local, tự đồng bộ sang server trước (git pull / scp) — script chỉ lo phần image.

## Cách chạy

```bash
DEPLOY_HOST=user@your-server \
DEPLOY_PATH=/path/to/backup-db-go \
./deploy/deploy.sh
```

- `DEPLOY_HOST`: đích SSH (user@host, hoặc alias trong `~/.ssh/config`).
- `DEPLOY_PATH`: thư mục chứa `docker-compose.yml` trên server.
- `DOCKER_PLATFORM` (tuỳ chọn): đặt `linux/amd64` nếu máy local kiến trúc khác server (vd build trên Mac Apple Silicon, deploy lên server x86_64).

Script sẽ tự:

1. `docker build` image `backupdb:latest` từ thư mục gốc repo.
2. `docker save` + gzip ra file tạm.
3. `scp` file đó lên server.
4. SSH vào server: `docker load` nạp image, xoá file tạm, rồi `cd "$DEPLOY_PATH" && docker compose up -d` để khởi động lại 3 service (`consumer`/`scheduler`/`admin`) với image mới — **không** dùng `--build`, vì image đã có sẵn từ bước load.

## Deploy `agent` — backup database chỉ server đó kết nối được, server chính không mở port

Dùng khi database nằm trên 1 server hoàn toàn khác (nhà cung cấp khác, qua Internet công khai) và server chính **không được phép mở port** để nhận kết nối vào — xem mục "Backup database trên server khác (agent)" ở README.md gốc để hiểu luồng hoạt động. `agent` không cần Redis/SQLite, nên deploy độc lập bằng script riêng `deploy-agent.sh`, không đụng gì tới `docker-compose.yml`/`deploy.sh` của server chính.

1. Trên server có database đó, clone repo này vào 1 thư mục riêng (không liên quan tới checkout của server chính, nếu có), rồi:
   - `.env` — điền tối thiểu `AGENT_TOKEN` (tự tạo, vd `openssl rand -hex 32`) và `AGENT_PORT` nếu muốn khác `8443`. Xem `.env.example` mục "agent subcommand only".
   - `google/credentials.json` — **chỉ cần nếu** có database qua agent này upload lên Google Drive (S3 thì bỏ qua, không cần file này).
   - `docker-compose.agent.yml` đã có sẵn trong repo, không cần đổi tên hay đụng gì tới nó — `deploy-agent.sh` tự chỉ định đúng file này bằng `docker compose -f`.
2. Mở port `AGENT_PORT` (mặc định `8443`) trong firewall **của chính server này** — server chính vẫn không cần mở gì cả.
3. Deploy từ máy local, trỏ vào thư mục vừa tạo trên server đó:
   ```bash
   DEPLOY_HOST=user@agent-server \
   DEPLOY_PATH=/path/to/agent-folder \
   ./deploy/deploy-agent.sh
   ```
   Các biến giống hệt `deploy.sh` (`DEPLOY_HOST`, `DEPLOY_PATH`, `DOCKER_PLATFORM` tuỳ chọn) — chỉ khác ở bước cuối, chạy `docker compose -f docker-compose.agent.yml up -d` thay vì `docker compose up -d`.
4. Script tự in ra lệnh xem log lúc xong — chạy nó để lấy dòng **"Agent certificate fingerprint"** ở lần chạy đầu tiên, copy giá trị này.
5. Vào admin UI (server chính) → trang **Agent từ xa** → Thêm agent: điền endpoint (`https://ip-hoặc-domain:AGENT_PORT`), token (giống hệt `AGENT_TOKEN`), và fingerprint vừa lấy.
6. Vào trang Sửa database cần backup từ xa → mục "Chạy backup trên" → chọn agent vừa thêm.

Deploy lại (cập nhật code) chỉ cần chạy lại bước 3 — cert/key agent đã lưu ở volume `agent-data` nên fingerprint giữ nguyên qua các lần deploy, không cần đăng ký lại trong admin UI.

## `mysql-host-proxy.sh` / `mongo-host-proxy.sh` — backup database cài trực tiếp trên server, không được sửa cấu hình

Bình thường, database cài trực tiếp trên server (không phải container) thì set `host` = `host.docker.internal` là backup được (xem README.md gốc). Nhưng nếu database đó đang chỉ nhận kết nối local (MySQL `bind-address = 127.0.0.1`, MongoDB `bindIp: 127.0.0.1`) và bạn **không có quyền sửa cấu hình**, `host.docker.internal` sẽ resolve đúng nhưng không connect được, vì traffic từ container tới bằng IP của Docker bridge, không phải `127.0.0.1`.

2 script này dựng 1 container `socat` chạy `network_mode: host` (dùng chung network namespace với server, nên gọi `127.0.0.1:<port>` y hệt database đang thấy) để forward traffic từ IP gateway của Docker bridge sang `127.0.0.1:<port>` — không đụng gì tới cấu hình database. Cùng 1 cơ chế, chỉ khác port mặc định — script riêng cho từng loại để chạy thẳng không cần chỉnh biến môi trường ở trường hợp thông thường.

Chạy ngay trên server đó (không cần build gì, không liên quan `deploy.sh`):

```bash
./deploy/mysql-host-proxy.sh   # MySQL/MariaDB
./deploy/mongo-host-proxy.sh   # MongoDB
```

Chạy lại bao nhiêu lần cũng được — script tự xoá container cũ (nếu có) rồi tạo lại, không báo lỗi "container đã tồn tại". Cần backup cả 2 loại trên cùng 1 server thì chạy cả 2 script, mỗi cái tự dùng tên container/port khác nhau, không đụng nhau.

Biến môi trường tuỳ chỉnh (đều có giá trị mặc định hợp lý):

| Biến | Mặc định (`mysql-host-proxy.sh`) | Mặc định (`mongo-host-proxy.sh`) | Ý nghĩa |
|---|---|---|---|
| `BIND_IP` | tự lấy qua `docker network inspect bridge` | (giống) | IP gateway Docker bridge để proxy lắng nghe — **không** dùng `0.0.0.0`, tránh lộ database ra ngoài |
| `LISTEN_PORT` | `33306` | `37017` | Port proxy lắng nghe, dùng làm `port` khi khai báo database trong admin UI |
| `TARGET_HOST` | `127.0.0.1` | `127.0.0.1` | Địa chỉ database thật đang nghe trên server |
| `TARGET_PORT` | `3306` | `27017` | Port database thật |

Postgres dùng chung cơ chế được (chỉ cần set `TARGET_PORT=5432` khi chạy `mysql-host-proxy.sh`, tên script không quan trọng), chưa có file riêng vì chưa có ai cần.

Sau khi chạy xong, vào admin UI khai báo database: `host` = `host.docker.internal`, `port` = giá trị `LISTEN_PORT` tương ứng ở trên.

## Lưu ý quan trọng

- **Tên project Docker Compose lấy theo tên thư mục** (không set `-p`/`COMPOSE_PROJECT_NAME`) — `DEPLOY_PATH` trên server phải là thư mục cùng tên với lúc deploy lần đầu (vd `backup-db-go`), nếu không `docker compose up -d` sẽ tạo project mới, container/volume rỗng khác hoàn toàn, dữ liệu cũ (SQLite, Redis) vẫn còn nhưng nằm ở volume của project cũ, không được dùng.
- Thay image không đụng tới volume `sqlite-data`/`redis-data` — an toàn để deploy lại nhiều lần, dữ liệu backup/lịch/cấu hình không mất.
- Muốn xem log sau khi build/deploy xong, vào trang **Nhật ký** trên admin UI thay vì `docker logs` — xem thêm README.md ở thư mục gốc.
