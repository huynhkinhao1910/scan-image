# Scan Image

App macOS tìm ảnh trên NAS nhanh kiểu Everything. Viết bằng Go + [MyGo](https://github.com/egoist/mygo) (native UI, không dùng webview).

Mỗi máy Mac chạy độc lập (cách A). Mở app sẽ gặp màn hình **Kết nối**:
- **Thư mục local**: chọn một thư mục bất kỳ trên máy.
- **Server NAS**: nhập IP/tên server (không hard code). App tự liệt kê các share account được phép truy cập (`smbutil view`), mount những share còn thiếu bằng mật khẩu trong Keychain (`osascript mount volume`), rồi quét tất cả share đã chọn vào chung một index. Các server đang mount sẵn hiện làm gợi ý.

Search chạy trong RAM và không đi qua mạng. Index được **lưu trên máy** (`~/Library/Application Support/ScanImage/index/`, không bao giờ ghi lên NAS), nên lần mở sau search được ngay, kể cả khi chưa bật VPN.

## Index lưu trên máy và chạy nền
- **Mở là search ngay**: app nhớ nguồn mở lần cuối, nạp index đã lưu (3 triệu mục khoảng 0,35 giây) rồi mới quét cập nhật ở nền.
- **Quét incremental**: SMB không có change journal, nên app đi lại cây thư mục nhưng bỏ qua thư mục lá có mtime không đổi. Mỗi lượt soát lại thêm khoảng 1/64 số thư mục lá để tự chữa những thay đổi mà mtime không thấy (file bị ghi đè tại chỗ). Lỗi mạng hay bấm Dừng giữa chừng không làm mất mục nào: phần chưa quét giữ dữ liệu cũ.
- **Chạy nền**: đóng cửa sổ là ẩn, app vẫn ở thanh menu (menu: Mở Scan Image, Quét ngay, Thoát). Tự quét lại mỗi 30 phút và 15 giây sau khi máy thức dậy. Cmd+Q mới thoát thật.
- **Mở cùng máy**: bật trong Settings → INDEX. macOS 13+ chỉ cho bật trên bản đã đóng gói (`go tool mygo build`), không bật được khi chạy `go tool mygo dev`.
- **Offline**: không tới được server thì app dùng index đã lưu và không bật hộp thoại mount nào.
- **Quét lại toàn bộ**: Settings → INDEX, dùng khi nghi index lệch với NAS.

## Đẩy file lên S3
Cấu hình trong Settings → S3 (bucket, region hoặc endpoint, access key; secret nằm trong Keychain). Mọi lần đẩy đều ghi vào bucket đã **Lưu** tại thời điểm bấm đẩy; sửa Settings sau đó không ảnh hưởng job đang chờ.

**Tab File hỏng**: sửa các file design bị hỏng hoặc thiếu trên S3 theo danh sách của server.
- "Lấy danh sách": tải danh sách từ URL trong Settings; để trống thì dùng API mặc định `https://production.trackingis.info/api/queue-s3-upload`. App đọc được hai dạng: JSON `[{"sku": "<tên file>", "path": "/thư mục/"}]` của API, và text `File<TAB>Path` mỗi dòng của file `missing_s3_files.txt` cũ. Chỉ nhận https.
- App tìm từng file theo **tên** trong index. Có file trùng tên chính xác thì dòng "Sẵn sàng". Nhiều file trùng tên thì app tự lấy bản sửa gần nhất, không hỏi: dòng ghi "Sẵn sàng · lấy bản mới nhất trong N bản" và cột nguồn hiện đường dẫn của bản đó. Chỉ giống sau khi bỏ dấu thì dòng báo "Tên gần giống, cần xác nhận" và phải bấm "Dùng file này" mới đẩy.
- "Tìm nhanh theo ngày": không chờ index. Tên như `S-121026A09.png` chứa ngày 12/10/26, app chỉ đi vào các thư mục đúng năm/tháng/ngày. Tên không có ngày vẫn phải chờ index.
- "Tìm file thiếu": chạy một lượt quét incremental.
- "Chạy thử": hiện `nguồn → key`, không ghi gì lên S3.
- "Đẩy tất cả": hỏi xác nhận một lần, **ghi đè** object cũ, đẩy xong kiểm lại dung lượng. Key = `Path` bỏ dấu `/` đầu + `File`, không thêm prefix. Ví dụ `/Etsy/Etsy-Mai/` + `C-071026A26.png` → `Etsy/Etsy-Mai/C-071026A26.png`.
- "Tự động (5 phút)": bật một lần (có hộp xác nhận ghi tên bucket), sau đó cứ 5 phút app tự lấy danh sách, khớp với index, tìm theo ngày các dòng còn thiếu, đẩy lên S3 rồi báo server. Lựa chọn được lưu, mở app lại là chạy tiếp, kể cả khi cửa sổ đang ẩn. Chỉ đẩy file trùng tên chính xác, có nhiều bản thì lấy bản sửa gần nhất; dòng "tên gần giống" vẫn chờ bấm "Dùng file này". Không đẩy khi index chưa quét xong lần đầu, và không đẩy lại cùng một file nguồn nếu server vẫn liệt kê nó sau khi đã đẩy. Ở chế độ này không ai xem đường dẫn trước khi đẩy: ai ghi được vào ổ mạng một file đúng tên đang thiếu thì file đó sẽ lên bucket, kể cả khi đã có bản cũ hơn cùng tên.
- Rê chuột vào tên file thì hiện nút copy tên. Cột "Đẩy lúc" ghi giờ file được đẩy xong.

**Tab Hàng đợi S3**: mọi file đã xếp hàng đẩy từ lúc mở app, mới nhất ở trên: tên file (có nút copy khi rê chuột), key trên S3, nguồn trên ổ mạng, trạng thái và giờ đẩy xong. Tên tab ghi số file đang chờ hoặc đang chạy. Dòng đã xong có nút mở file trên web (địa chỉ không có chữ ký, chỉ xem được khi bucket cho đọc công khai). Các nút "Huỷ hàng đợi", "Thử lại lỗi", "Xoá đã xong" nằm ở đây. Danh sách không được lưu: thoát app là mất.

**Chống file rỗng (hình trắng)**: có tiến trình khác cũng ghi vào bucket và từng đè file rỗng lên file app vừa đẩy. App chặn ở ba chỗ:
- File nguồn 0 byte không bao giờ được đẩy. Ở tab File hỏng dòng đó báo "File nguồn rỗng (0 byte), không đẩy".
- Đẩy xong, app hỏi lại dung lượng trên S3 và chỉ báo "Xong" khi khớp với file nguồn.
- 1 phút và 10 phút sau khi đẩy, app hỏi lại dung lượng lần nữa. Trạng thái ghi "chờ kiểm lại" rồi "đã kiểm lại". Nếu file trên S3 đã đổi dung lượng hoặc biến mất, app tự đẩy lại, tối đa 3 lần; quá số đó dòng chuyển thành lỗi "có tiến trình khác ghi đè key này". "Xoá đã xong" giữ lại các dòng còn chờ kiểm. Thoát app thì các lần kiểm còn chờ bị bỏ.

**Tab Tìm ảnh**: chọn file bằng ô tick ở đầu mỗi dòng, bằng "Chọn tất cả" (mọi dòng đang hiện, tối đa 2.000), hoặc Cmd/Shift-click, rồi "Upload S3". Một popup mở ra để chọn thư mục đích trong bucket (app quét sẵn các thư mục của bucket, gõ để lọc, gõ tay thư mục mới cũng được). Key = `thư mục đã chọn + tên file`. Mục "Theo cấu trúc share (mặc định)" giữ cách cũ: `<prefix>/<tên share>/<đường dẫn con>`. File đã có trên bucket thì app hỏi trước khi ghi đè. Xếp hàng xong app chuyển sang tab Hàng đợi S3. File nào được đẩy đúng vào key đang có trong danh sách file hỏng (cùng thư mục, cùng tên) thì đẩy xong app báo server như khi đẩy từ tab File hỏng; muốn vậy danh sách phải đã được lấy trước khi bấm đẩy.

## Yêu cầu
- macOS, **Go ≥ 1.27.1** (MyGo v0.4.6 yêu cầu): `brew install go` hoặc tải ở go.dev
- NAS đã mount, ví dụ `/Volumes/Photos` (qua Kerio VPN cũng được)

## Chạy lần đầu
```bash
cd scan-image
go mod tidy                      # tải mygo, tạo go.sum
go test -race ./internal/...     # test lõi
go run ./cmd/nasprobe -path /Volumes/<NAS> -limit 20000 -workers 8,32,64   # P0: đo tốc độ
go run ./cmd/nasprobe -path /Volumes/<NAS> -lister readdir,bulk            # so lister thường với getattrlistbulk
go tool mygo dev                 # chạy app (nếu chưa có: go get -tool github.com/egoist/mygo/cmd/mygo)
```

Máy chưa cài Xcode Command Line Tools thì thêm `CGO_ENABLED=0` trước các lệnh `go` (hoặc chạy `xcode-select --install`): `purego` kéo theo `runtime/cgo` khi cgo bật.

App quét bằng lister `getattrlistbulk` (`source.LocalFS{Bulk: true}`): một lệnh lấy cả tên, loại, dung lượng và ngày sửa của một thư mục, thay vì một `lstat` qua mạng cho mỗi file. Đo trên NAS thật qua VPN: 2.000–4.000 mục/giây, lister cũ khoảng 250. `nasprobe -lister readdir,bulk` so hai cách và in ra thư mục nào không đọc được; số đo trên đĩa local không có ý nghĩa vì page cache.

## Cấu trúc
```
main.go / app.go / view.go   UI MyGo: kết nối, quét, cards, search, bảng kết quả, Settings xổ xuống
tray.go                      icon thanh menu và menu chạy nền
icons/                       icon Lucide (ISC) nhúng vào binary
upload.go                    hàng đợi upload S3 (8 file song song, tiến độ, huỷ, thử lại)
queue_view.go                tab Hàng đợi S3, nút copy tên file
recheck.go                   kiểm lại file trên S3 sau khi đẩy, đẩy lại nếu bị ghi đè
broken*.go                   tab File hỏng: danh sách, khớp tên, tìm theo ngày, đẩy
dest*.go                     popup chọn thư mục đích S3
resources/icon.png           icon app (MyGo tự build ra .icns)
internal/netshare            liệt kê / mount share SMB của server (mount, smbutil, osascript)
internal/s3up                upload S3 / S3-compatible: SigV4 tự viết, PUT + multipart, retry, liệt kê thư mục bucket, secret trong Keychain
internal/brokenlist          tải và validate danh sách file hỏng, ghép key S3
internal/datefind            tìm file theo ngày trong tên, chỉ đi các thư mục khớp ngày
internal/source              interface Source + adapter LocalFS (NAS đã mount), lister getattrlistbulk tuỳ chọn
internal/scanner             quét song song nhiều worker (giấu latency VPN), đếm tiến độ, huỷ là dừng ngay
internal/search              index trong RAM: arena []byte, bỏ dấu tiếng Việt, quét song song; snapshot nhị phân; Builder dựng index mới từ index cũ
internal/indexer             điều phối: lưu/nạp snapshot, session, quét incremental theo lịch, trạng thái cho UI
internal/fold                lowercase + bỏ dấu ("Ảnh Cá" → "anh ca")
cmd/nasprobe                 CLI đo số mục/giây khi liệt kê NAS theo số worker
```

## Trạng thái
- [x] P0 `nasprobe`
- [x] Scanner song song + index RAM + search (bench: 3 triệu tên ≈ 22ms trên 2 core)
- [x] UI cơ bản: cards, search, bảng, Mở file / Show in Finder
- [x] P1 lưu index trên máy (snapshot nhị phân, không dùng SQLite), mở là search ngay, quét incremental bỏ qua thư mục lá mtime không đổi, chạy nền
- [x] P4 bỏ view Lưới + thumbnail (đọc cả file PSD qua VPN tranh băng thông với quét)
- [x] Upload S3: chọn nhiều file → "Upload S3" → chọn thư mục đích trong bucket
- [x] Tab File hỏng: lấy danh sách của server, khớp tên, tìm nhanh theo ngày, đẩy lại đúng key
- [ ] P5 đóng gói `.dmg`, ký + notarize
