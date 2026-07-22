# BackupDB

Service backup MySQL/PostgreSQL/MongoDB → Google Drive/S3, viết bằng Go, deploy bằng Docker.

Tính năng chính:

- Kết nối MySQL/PostgreSQL/MongoDB đích qua **TCP trực tiếp** (host/port/user/pass) — không cần `docker exec`, không cần mount `/var/run/docker.sock`. Database đích có thể là container khác trên network `dbnet` (host = tên container) hoặc cài trực tiếp trên server chạy Docker — nhập `host` là `localhost`/`127.0.0.1` hoặc `host.docker.internal` đều được, `consumer` tự hiểu `localhost`/`127.0.0.1` (vốn chỉ trỏ vào chính container, không có database nào) là `host.docker.internal` (đã cấu hình sẵn `extra_hosts` trong `docker-compose.yml`).
- Danh sách database cần backup, lịch backup, nơi lưu trữ, và kênh thông báo đều nằm trong **SQLite**, quản lý qua **giao diện web** (`admin`).
- **Lịch backup lưu trong SQLite**, mỗi database có thể có nhiều giờ backup/ngày, quản lý ngay trên trang Sửa database — không dùng crontab. Ngoài lịch riêng từng database còn có **lịch chung** (trang "Lịch chung"): 1 nhóm database dùng chung bất kỳ số khung giờ nào, không cần lặp lại cấu hình cho từng database. Giờ trong mọi lịch diễn giải theo 1 timezone chung cho cả deployment (`SCHEDULER_TIMEZONE`, mặc định `Asia/Ho_Chi_Minh`).
- **Nhiều nơi lưu trữ cùng lúc**: có thể kết nối nhiều tài khoản Google Drive và nhiều cấu hình S3 (AWS S3, MinIO, R2, Spaces...), mỗi database tự chọn upload vào đâu, quản lý ở trang "Nơi lưu trữ" trong admin UI.
- **Nhiều kênh thông báo**: kênh Telegram (gửi thẳng qua Bot API, không qua relay trung gian) hôm nay, thêm loại kênh khác sau này — mỗi database tự chọn bất kỳ số kênh nào, quản lý ở trang "Thông báo" trong admin UI.
- **Nhật ký backup** ngay trên web (trang "Nhật ký"): mỗi job `consumer` xử lý xong (thành công hoặc lỗi) đều được ghi vào SQLite — không cần `docker logs`. Xem được database, driver, thời lượng, thông báo lỗi; có nút xoá toàn bộ khi muốn dọn.
- Worker (`consumer`) chạy vô hạn, không tự thoát sau N job; cũng không cần chờ có nơi lưu trữ nào được cấu hình mới chạy được — job nào chưa có đích hợp lệ thì chỉ job đó báo lỗi qua các kênh thông báo đã gán, các job khác vẫn xử lý bình thường.

## Kiến trúc

```
redis      — hàng đợi job (RPUSH/BLPOP)
admin      — web UI: quản lý database, lịch backup (riêng + chung), nơi lưu trữ, kênh thông báo (Basic Auth)
scheduler  — mỗi 30s kiểm tra SQLite (lịch riêng + lịch chung), đến giờ thì đẩy job vào Redis
consumer   — lấy job từ Redis, dump (mysqldump/pg_dump/mongodump), gzip, upload, ghi kết quả vào nhật ký (SQLite) và gửi thông báo qua các kênh đã gán cho database đó
```

`admin`, `scheduler`, `consumer` dùng chung 1 file SQLite (volume `sqlite-data`, chế độ WAL để đọc/ghi đồng thời an toàn). `consumer` cần cùng Docker network với các container database đích để phân giải hostname (network `dbnet`).

Nơi lưu trữ (`internal/storage`) đứng sau 1 interface `Provider` — mỗi database chọn 1 "storage target" (1 hàng trong bảng `storage_targets`), gồm 2 loại:
- `gdrive` — 1 tài khoản Google Drive đã đăng nhập, token OAuth lưu ngay trong SQLite (không phải file), tự refresh khi hết hạn.
- `s3` — 1 bucket S3-compatible (AWS S3, MinIO, Cloudflare R2, DigitalOcean Spaces...), cấu hình endpoint/bucket/access key lưu trong SQLite.

Thêm loại đích mới sau này chỉ cần thêm 1 package implement `Provider` + 1 case trong `internal/storage.New`.

## Cài đặt lần đầu

1. Copy `.env.example` → `.env`, điền `ADMIN_USERNAME`/`ADMIN_PASSWORD` (và `SCHEDULER_TIMEZONE` nếu không dùng giờ Việt Nam).
2. Đặt `google/credentials.json` (OAuth client tải từ Google Cloud Console, loại "Desktop app") vào thư mục `docker/google/`. File này là **danh tính của app**, dùng chung cho mọi tài khoản Google bạn kết nối — không phải thông tin đăng nhập.
3. Tạo network dùng chung với các container database cần backup (nếu chưa có), rồi join các container đó vào:
   ```bash
   docker network create dbnet
   docker network connect dbnet mysql57   # lặp lại cho từng container DB cần backup
   ```
4. Khởi động:
   ```bash
   docker compose up -d --build
   ```
5. Mở `http://<host>:8080` (Basic Auth) → vào trang **Nơi lưu trữ** → kết nối ít nhất 1 tài khoản Google Drive hoặc 1 cấu hình S3 (làm theo hướng dẫn trên trang, copy verification code từ URL trình duyệt sau khi Allow — không cần trang callback nào chạy được, chỉ cần copy code trên thanh địa chỉ).
6. Quay lại trang danh sách → thêm database (tên, driver, host, port, user, pass, chọn nơi lưu trữ) → vào trang Sửa để thêm giờ backup trong ngày (hoặc dùng trang "Lịch chung" nếu muốn nhiều database dùng chung 1 lịch).
7. (Tuỳ chọn) Vào trang **Thông báo** → thêm kênh Telegram (bot token + chat id, tạo bot qua @BotFather) → gán kênh cho database ở trang Sửa database để nhận báo lỗi/thành công.

## Chuyển từ `databases.txt` cũ

```bash
docker compose run --rm admin migrate /path/to/databases.txt
```

Định dạng cũ chỉ có tên container (không có port, không có nơi lưu trữ), nên sau khi import cần vào giao diện admin bổ sung host/port thật, chọn nơi lưu trữ, và thêm lịch backup cho từng database.

## Lệnh CLI (`backupdb <subcommand>`)

| Lệnh | Việc làm |
|---|---|
| `backup [dbname driver params]` | Không tham số: đẩy job cho mọi database đang bật; có tham số: đẩy 1 job thủ công |
| `consumer` | Vòng lặp worker |
| `scheduler` | Vòng lặp kiểm tra lịch trong SQLite, đẩy job khi tới giờ |
| `admin` | Web UI quản lý database, lịch (riêng + chung), nơi lưu trữ, kênh thông báo |
| `upload <dbname> <filepath> <filename> [targetID]` | Upload thủ công 1 file, mặc định dùng nơi lưu trữ đã gán cho `dbname` trong registry |
| `migrate <databases.txt>` | Import file cấu hình cũ vào SQLite |

Kết nối tài khoản Google Drive / cấu hình S3 chỉ làm được qua trang **Nơi lưu trữ** trong admin UI, không còn lệnh CLI `login` riêng.

## Giới hạn đã biết / cần lưu ý khi vận hành thật

- Mật khẩu database, secret key S3, và thông tin kênh thông báo (vd bot token Telegram) lưu dạng plaintext trong SQLite (tương đương mức bảo mật của `databases.txt` cũ) — không expose port `admin` ra Internet công khai, chỉ dùng trong mạng nội bộ/VPN dù đã có Basic Auth.
- Schema SQLite hiện tại **không có migration tự động** giữa các phiên bản — nếu cấu trúc bảng đổi trong tương lai, xoá volume `sqlite-data` và cấu hình lại từ đầu (hoặc tự viết script migrate) thay vì kỳ vọng nâng cấp tại chỗ.
