# backupdb (Go rewrite)

Viết lại bằng Go cho service backup MySQL/PostgreSQL/MongoDB → Google Drive/S3 ở thư mục gốc repo, để deploy bằng Docker. **Độc lập hoàn toàn với code PHP ở root repo** — không sửa, không phụ thuộc lẫn nhau, có thể chạy song song trong lúc chuyển đổi.

Khác biệt chính so với bản PHP:

- Kết nối MySQL/PostgreSQL/MongoDB đích qua **TCP trực tiếp** (host/port/user/pass), không còn `docker exec` — không cần mount `/var/run/docker.sock`.
- Danh sách database cần backup, lịch backup, và nơi lưu trữ đều nằm trong **SQLite**, quản lý qua **giao diện web** (`admin`) — không còn sửa tay `databases.txt`.
- **Lịch backup lưu trong SQLite**, mỗi database có thể có nhiều giờ backup/ngày, quản lý ngay trên trang Sửa database — không dùng crontab.
- **Nhiều nơi lưu trữ cùng lúc**: có thể kết nối nhiều tài khoản Google Drive và nhiều cấu hình S3 (AWS S3, MinIO, R2, Spaces...), mỗi database tự chọn upload vào đâu, quản lý ở trang "Nơi lưu trữ" trong admin UI.
- Worker (`consumer`) chạy vô hạn, không tự thoát sau N job như bản PHP; cũng không cần chờ có nơi lưu trữ nào được cấu hình mới chạy được — job nào chưa có đích hợp lệ thì chỉ job đó báo lỗi (Telegram alert), các job khác vẫn xử lý bình thường.

## Kiến trúc

```
redis      — hàng đợi job (RPUSH/BLPOP)
admin      — web UI: quản lý database, lịch backup, nơi lưu trữ (Basic Auth)
scheduler  — mỗi 30s kiểm tra SQLite, đến giờ thì đẩy job vào Redis
consumer   — lấy job từ Redis, dump (mysqldump/pg_dump/mongodump), gzip, upload, báo Telegram
```

`admin`, `scheduler`, `consumer` dùng chung 1 file SQLite (volume `sqlite-data`, chế độ WAL để đọc/ghi đồng thời an toàn). `consumer` cần cùng Docker network với các container database đích để phân giải hostname (network `dbnet`).

Nơi lưu trữ (`internal/storage`) đứng sau 1 interface `Provider` — mỗi database chọn 1 "storage target" (1 hàng trong bảng `storage_targets`), gồm 2 loại:
- `gdrive` — 1 tài khoản Google Drive đã đăng nhập, token OAuth lưu ngay trong SQLite (không phải file), tự refresh khi hết hạn.
- `s3` — 1 bucket S3-compatible (AWS S3, MinIO, Cloudflare R2, DigitalOcean Spaces...), cấu hình endpoint/bucket/access key lưu trong SQLite.

Thêm loại đích mới sau này chỉ cần thêm 1 package implement `Provider` + 1 case trong `internal/storage.New`.

## Cài đặt lần đầu

1. Copy `.env.example` → `.env`, điền `TELEGRAM_*`, `ADMIN_USERNAME`/`ADMIN_PASSWORD`.
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
6. Quay lại trang danh sách → thêm database (tên, driver, host, port, user, pass, chọn nơi lưu trữ) → vào trang Sửa để thêm giờ backup trong ngày.

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
| `admin` | Web UI quản lý database, lịch, nơi lưu trữ |
| `upload <dbname> <filepath> <filename> [targetID]` | Upload thủ công 1 file, mặc định dùng nơi lưu trữ đã gán cho `dbname` trong registry |
| `migrate <databases.txt>` | Import file cấu hình cũ vào SQLite |

Kết nối tài khoản Google Drive / cấu hình S3 chỉ làm được qua trang **Nơi lưu trữ** trong admin UI, không còn lệnh CLI `login` riêng.

## Giới hạn đã biết / cần lưu ý khi vận hành thật

- Mật khẩu database và secret key S3 lưu dạng plaintext trong SQLite (tương đương mức bảo mật của `databases.txt` cũ) — không expose port `admin` ra Internet công khai, chỉ dùng trong mạng nội bộ/VPN dù đã có Basic Auth.
- Schema SQLite hiện tại **không có migration tự động** giữa các phiên bản — nếu cấu trúc bảng đổi trong tương lai, xoá volume `sqlite-data` và cấu hình lại từ đầu (hoặc tự viết script migrate) thay vì kỳ vọng nâng cấp tại chỗ.
