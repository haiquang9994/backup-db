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

## `mysql-host-proxy.sh` — backup database cài trực tiếp trên server, không được sửa cấu hình MySQL

Bình thường, database cài trực tiếp trên server (không phải container) thì set `host` = `host.docker.internal` là backup được (xem README.md gốc). Nhưng nếu MySQL trên server đó đang `bind-address = 127.0.0.1` (chỉ nhận kết nối local) và bạn **không có quyền sửa cấu hình MySQL**, `host.docker.internal` sẽ resolve đúng nhưng không connect được, vì traffic từ container tới bằng IP của Docker bridge, không phải `127.0.0.1`.

`mysql-host-proxy.sh` dựng 1 container `socat` chạy `network_mode: host` (dùng chung network namespace với server, nên gọi `127.0.0.1:3306` y hệt MySQL đang thấy) để forward traffic từ IP gateway của Docker bridge sang `127.0.0.1:3306` — không đụng gì tới MySQL.

Chạy ngay trên server đó (không cần build gì, không liên quan `deploy.sh`):

```bash
./deploy/mysql-host-proxy.sh
```

Chạy lại bao nhiêu lần cũng được — script tự xoá container cũ (nếu có) rồi tạo lại, không báo lỗi "container đã tồn tại".

Biến môi trường tuỳ chỉnh (đều có giá trị mặc định hợp lý):

| Biến | Mặc định | Ý nghĩa |
|---|---|---|
| `BIND_IP` | tự lấy qua `docker network inspect bridge` | IP gateway Docker bridge để proxy lắng nghe — **không** dùng `0.0.0.0`, tránh lộ MySQL ra ngoài |
| `LISTEN_PORT` | `33306` | Port proxy lắng nghe, dùng làm `port` khi khai báo database trong admin UI |
| `TARGET_HOST` | `127.0.0.1` | Địa chỉ MySQL thật đang nghe trên server |
| `TARGET_PORT` | `3306` | Port MySQL thật |

Sau khi chạy xong, vào admin UI khai báo database: `host` = `host.docker.internal`, `port` = `33306` (hoặc `$LISTEN_PORT` nếu bạn đổi).

## Lưu ý quan trọng

- **Tên project Docker Compose lấy theo tên thư mục** (không set `-p`/`COMPOSE_PROJECT_NAME`) — `DEPLOY_PATH` trên server phải là thư mục cùng tên với lúc deploy lần đầu (vd `backup-db-go`), nếu không `docker compose up -d` sẽ tạo project mới, container/volume rỗng khác hoàn toàn, dữ liệu cũ (SQLite, Redis) vẫn còn nhưng nằm ở volume của project cũ, không được dùng.
- Thay image không đụng tới volume `sqlite-data`/`redis-data` — an toàn để deploy lại nhiều lần, dữ liệu backup/lịch/cấu hình không mất.
- Muốn xem log sau khi build/deploy xong, vào trang **Nhật ký** trên admin UI thay vì `docker logs` — xem thêm README.md ở thư mục gốc.
