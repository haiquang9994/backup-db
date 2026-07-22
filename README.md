# BackupDB

Service backup MySQL/PostgreSQL/MongoDB → Google Drive/S3, viết bằng Go, deploy bằng Docker.

Tính năng chính:

- Kết nối MySQL/PostgreSQL/MongoDB đích qua **TCP trực tiếp** (host/port/user/pass) — không cần `docker exec`, không cần mount `/var/run/docker.sock`. Database đích có thể là container khác trên network `dbnet` (host = tên container) hoặc cài trực tiếp trên server chạy Docker — nhập `host` là `localhost`/`127.0.0.1` hoặc `host.docker.internal` đều được, `consumer` tự hiểu `localhost`/`127.0.0.1` (vốn chỉ trỏ vào chính container, không có database nào) là `host.docker.internal` (đã cấu hình sẵn `extra_hosts` trong `docker-compose.yml`).
- Danh sách database cần backup, lịch backup, nơi lưu trữ, và kênh thông báo đều nằm trong **SQLite**, quản lý qua **giao diện web** (`admin`).
- **Lịch backup lưu trong SQLite**, mỗi database có thể có nhiều giờ backup/ngày, quản lý ngay trên trang Sửa database — không dùng crontab. Ngoài lịch riêng từng database còn có **lịch chung** (trang "Lịch chung"): 1 nhóm database dùng chung bất kỳ số khung giờ nào, không cần lặp lại cấu hình cho từng database. Giờ trong mọi lịch diễn giải theo 1 timezone chung cho cả deployment (`SCHEDULER_TIMEZONE`, mặc định `Asia/Ho_Chi_Minh`).
- **Nhiều nơi lưu trữ cùng lúc**: có thể kết nối nhiều tài khoản Google Drive và nhiều cấu hình S3 (AWS S3, MinIO, R2, Spaces...), mỗi database tự chọn upload vào đâu, quản lý ở trang "Nơi lưu trữ" trong admin UI.
- **Nhiều kênh thông báo**: kênh Telegram (gửi thẳng qua Bot API, không qua relay trung gian) hôm nay, thêm loại kênh khác sau này — mỗi database tự chọn bất kỳ số kênh nào, quản lý ở trang "Thông báo" trong admin UI.
- **Nhật ký backup** ngay trên web (trang "Nhật ký"): mỗi job `consumer` xử lý xong (thành công hoặc lỗi) đều được ghi vào SQLite — không cần `docker logs`. Xem được database, driver, thời lượng, thông báo lỗi; có nút xoá toàn bộ khi muốn dọn.
- **Danh sách file đã backup** cho từng database (nút "File" ở trang danh sách database): xem tên file, dung lượng, thời gian upload, và tải trực tiếp về máy — S3 chuyển hướng thẳng tới link tải có chữ ký (không qua server admin), Google Drive tải qua server admin bằng chính token đã kết nối.
- **Backup database nằm trên server khác** (trang "Agent từ xa"), cho trường hợp deployment chính không được phép mở port nào để nhận kết nối vào: chạy `backupdb agent` (HTTPS, TLS tự ký, xác thực bằng token) ngay trên server có database đó, khai báo endpoint + token + cert fingerprint trong admin UI, rồi gán agent đó cho database ở trang Sửa database (mục "Chạy backup trên"). Mọi kết nối đều do server chính chủ động gọi ra (đẩy job, rồi tự hỏi kết quả) — không cần mở bất kỳ port nào trên server chính. Xem chi tiết ở mục "Backup database trên server khác (agent)" bên dưới.
- Worker (`consumer`) chạy vô hạn, không tự thoát sau N job; cũng không cần chờ có nơi lưu trữ nào được cấu hình mới chạy được — job nào chưa có đích hợp lệ thì chỉ job đó báo lỗi qua các kênh thông báo đã gán, các job khác vẫn xử lý bình thường.

## Kiến trúc

```
redis      — hàng đợi job (RPUSH/BLPOP)
admin      — web UI: quản lý database, lịch backup (riêng + chung), nơi lưu trữ, kênh thông báo (Basic Auth)
scheduler  — mỗi 30s kiểm tra SQLite (lịch riêng + lịch chung), đến giờ thì đẩy job vào Redis
consumer   — lấy job từ Redis, dump (mysqldump/pg_dump/mongodump), gzip, upload, ghi kết quả vào nhật ký + danh sách file (SQLite) và gửi thông báo qua các kênh đã gán cho database đó
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
| `admin` | Web UI quản lý database, lịch (riêng + chung), nơi lưu trữ, kênh thông báo, agent từ xa |
| `agent` | Chạy trên server khác (xem mục bên dưới): server HTTPS đứng chờ nhận job dump+upload, không cần SQLite/Redis |
| `upload <dbname> <filepath> <filename> [targetID]` | Upload thủ công 1 file, mặc định dùng nơi lưu trữ đã gán cho `dbname` trong registry |
| `migrate <databases.txt>` | Import file cấu hình cũ vào SQLite |

Kết nối tài khoản Google Drive / cấu hình S3 chỉ làm được qua trang **Nơi lưu trữ** trong admin UI, không còn lệnh CLI `login` riêng.

## Backup database trên server khác (agent)

Bình thường `consumer` kết nối thẳng TCP tới database đích, nên chỉ cần cùng network Docker (`dbnet`) hoặc `host.docker.internal` là đủ. Nhưng nếu database đó nằm trên **1 server hoàn toàn khác** (nhà cung cấp khác, qua Internet công khai) và server chính **không được phép mở port nào** để nhận kết nối vào, dùng `agent`:

1. Trên server có database đó, chạy `backupdb agent` (cần biến môi trường `AGENT_TOKEN` — 1 chuỗi bí mật tự đặt; tuỳ chọn `AGENT_PORT`, mặc định `8443`). Lần chạy đầu tiên nó tự sinh 1 chứng chỉ TLS tự ký, in ra **fingerprint** ở log — copy lại giá trị này.
2. Trong admin UI (server chính) → trang **Agent từ xa** → Thêm agent: điền tên gợi nhớ, endpoint (`https://ip-hoặc-domain:port`), token (giống hệt `AGENT_TOKEN` đã đặt), và cert fingerprint vừa copy.
3. Vào trang Sửa database cần backup từ xa → mục "Chạy backup trên" → chọn agent vừa thêm.

Từ lúc này, `scheduler`/`backup`/nút "Backup ngay" vẫn hoạt động y hệt — job vẫn qua Redis nội bộ như thường. Chỉ khác ở bước cuối: `consumer` phát hiện database này có gán agent thì **tự gọi ra** endpoint đó (đẩy job, rồi định kỳ hỏi kết quả) thay vì tự dump+upload — không có bất kỳ kết nối nào đi *vào* server chính, mọi kết nối đều do server chính chủ động mở ra. Agent tự dump + upload bằng đúng thông tin nơi lưu trữ được gửi kèm trong job, không cần đọc SQLite hay Redis của server chính.

Job giao cho agent chạy nền song song với các database khác (local hoặc agent khác) — không bị chặn chờ. Nhưng **trong cùng 1 agent**, nếu agent đó phụ trách nhiều database, các job sẽ xếp hàng và chạy **tuần tự từng cái một** (không chạy song song), tránh nhiều `mysqldump`/`pg_dump`/`mongodump` cùng lúc đè CPU/network của chính server đó — giống hệt cách `consumer` xử lý database local.

Nếu agent chạy trong Docker (khuyến nghị, dùng chung image `backupdb:latest`), thêm `extra_hosts: host.docker.internal:host-gateway` như service `consumer` để backup được cả database cài trực tiếp trên chính server đó.

## Giới hạn đã biết / cần lưu ý khi vận hành thật

- Mật khẩu database, secret key S3, thông tin kênh thông báo (vd bot token Telegram), và token của agent từ xa đều lưu dạng plaintext trong SQLite (tương đương mức bảo mật của `databases.txt` cũ) — không expose port `admin` ra Internet công khai, chỉ dùng trong mạng nội bộ/VPN dù đã có Basic Auth.
- Chứng chỉ TLS của `agent` là tự ký (self-signed), xác thực bằng cách ghim đúng fingerprint (không qua CA công cộng) — nếu 1 agent bị đổi cert (vd cài lại server, xoá nhầm file cert/key) thì phải cập nhật lại fingerprint mới trong admin UI, các job cũ sẽ báo lỗi "certificate fingerprint mismatch" cho tới lúc đó thay vì âm thầm tin tưởng 1 cert lạ.
- Schema SQLite hiện tại **không có migration tự động** giữa các phiên bản — nếu cấu trúc bảng đổi trong tương lai, xoá volume `sqlite-data` và cấu hình lại từ đầu (hoặc tự viết script migrate) thay vì kỳ vọng nâng cấp tại chỗ.
