# Scan Image

App macOS tìm ảnh trên NAS nhanh kiểu Everything. Viết bằng Go + [MyGo](https://github.com/egoist/mygo) (native UI, không dùng webview).

Mỗi máy Mac chạy độc lập (cách A). Mở app sẽ gặp màn hình **Kết nối**:
- **Thư mục local**: chọn một thư mục bất kỳ trên máy.
- **Server NAS**: nhập IP/tên server (không hard code). App tự liệt kê các share account được phép truy cập (`smbutil view`), mount những share còn thiếu bằng mật khẩu trong Keychain (`osascript mount volume`), rồi quét tất cả share đã chọn vào chung một index. Các server đang mount sẵn hiện làm gợi ý.

Index nằm trong RAM, search không đi qua mạng.

## Yêu cầu
- macOS, **Go ≥ 1.27.1** (MyGo v0.4.6 yêu cầu): `brew install go` hoặc tải ở go.dev
- NAS đã mount, ví dụ `/Volumes/Photos` (qua Kerio VPN cũng được)

## Chạy lần đầu
```bash
cd scan-image
go mod tidy                      # tải mygo, tạo go.sum
go test ./internal/...           # test lõi
go run ./cmd/nasprobe -path /Volumes/<NAS> -limit 20000 -workers 8,32,64   # P0: đo tốc độ
go tool mygo dev                 # chạy app (nếu chưa có: go get -tool github.com/egoist/mygo/cmd/mygo)
```

## Cấu trúc
```
main.go / app.go / view.go   UI MyGo: kết nối, quét, cards, search, bảng kết quả, Settings xổ xuống
thumbs.go                    hàng đợi thumbnail cho view Lưới (ưu tiên ảnh đang thấy, 8 worker, giữ 1.500 ảnh trong RAM)
icons/                       icon Lucide (ISC) nhúng vào binary
upload.go                    hàng đợi upload S3 (3 file song song, tiến độ, huỷ, thử lại)
resources/icon.png           icon app (MyGo tự build ra .icns)
internal/netshare            liệt kê / mount share SMB của server (mount, smbutil, osascript)
internal/s3up                upload S3 / S3-compatible: SigV4 tự viết, PUT + multipart, retry, secret trong Keychain
internal/thumb               thumbnail: cache local → preview nhúng trong 128KB đầu file (JPEG EXIF, RAW) → Quick Look
internal/source              interface Source + adapter LocalFS (NAS đã mount)
internal/scanner             quét song song nhiều worker (giấu latency VPN), đếm tiến độ
internal/search              index trong RAM: arena []byte, bỏ dấu tiếng Việt, quét song song
internal/fold                lowercase + bỏ dấu ("Ảnh Cá" → "anh ca")
cmd/nasprobe                 CLI đo số mục/giây khi liệt kê NAS theo số worker
```

## Trạng thái
- [x] P0 `nasprobe`
- [x] Scanner song song + index RAM + search (bench: 3 triệu tên ≈ 22ms trên 2 core)
- [x] UI cơ bản: cards, search, bảng, Mở file / Show in Finder
- [ ] P1 lưu index SQLite (plugin `github.com/egoist/mygo/plugins/sqlite`), resume, bỏ qua thư mục mtime không đổi
- [x] P4 view Lưới + thumbnail (preview nhúng, Quick Look), cache local
- [x] Upload S3: chọn nhiều file (Cmd/Shift-click) → "Upload S3"; key = `<prefix>/<tên share>/<đường dẫn con>`
- [ ] P5 đóng gói `.dmg`, ký + notarize
