# Scan Image

**Tìm file trên NAS nhanh như Everything, ngay trên macOS.** Gõ tên là ra kết quả trong vài chục mili giây trên vài triệu file, kể cả khi NAS nằm sau VPN.

Finder và Spotlight tìm rất chậm trên ổ mạng SMB, vì mỗi lần tìm lại phải hỏi server. Scan Image quét cây thư mục một lần, giữ index trong RAM và lưu trên máy, rồi chỉ quét lại những nhánh có thay đổi. Tìm kiếm không đi qua mạng, nên vẫn dùng được khi chưa bật VPN.

App viết bằng Go và [MyGo](https://github.com/egoist/mygo) (native UI, không dùng webview), không cần cài gì lên NAS. Mỗi máy Mac chạy độc lập: tự quét, tự giữ index của mình.

## Tính năng

| Tính năng | Làm gì |
|---|---|
| Kết nối | Chọn một thư mục trên máy, hoặc nhập IP/tên server NAS để app tự mount các share (giống Cmd+K của Finder). |
| Tìm ảnh | Gõ tên là ra kết quả trong vài chục mili giây trên vài triệu file, không phân biệt hoa thường và dấu tiếng Việt. Tìm được cả khi chưa bật VPN. |
| Index lưu trên máy | Kết quả quét được lưu lại, lần mở sau tìm được ngay rồi app mới quét cập nhật ở nền. |
| Tự quét lại | Mỗi 30 phút, khi mở app và khi máy thức dậy. Chỉ đi vào các nhánh có thay đổi gần đây. |
| Chạy nền | Đóng cửa sổ là ẩn xuống thanh menu, app vẫn quét định kỳ. Có thể bật mở cùng máy. |
| Upload S3 | Chọn file trong kết quả tìm, chọn thư mục đích trong bucket, đẩy lên. |
| File hỏng | Lấy danh sách file design hỏng hoặc thiếu trên S3 từ server, tìm file gốc trên NAS, đẩy lại đúng key rồi báo server. Có chế độ tự động 5 phút một lần. |
| Hàng đợi S3 | Theo dõi mọi file đang và đã đẩy, huỷ, thử lại, kiểm lại sau khi đẩy. |

## Bắt đầu nhanh

Yêu cầu: macOS và **Go ≥ 1.27.1** (MyGo v0.4.6 yêu cầu; `brew install go` hoặc tải ở go.dev).

```bash
git clone https://github.com/huynhkinhao1910/scan-image.git
cd scan-image
go tool mygo dev                                # chạy app
go tool mygo build -platform darwin/universal   # đóng gói .app + .dmg (Apple Silicon và Intel) vào build/
```

Mở app, chọn một thư mục trên máy hoặc nhập IP của NAS, chờ lượt quét đầu, rồi gõ tên file để tìm.

- Máy chưa cài Xcode Command Line Tools thì thêm `CGO_ENABLED=0` trước các lệnh `go` (hoặc chạy `xcode-select --install`): `purego` kéo theo `runtime/cgo` khi cgo bật.
- Bản đóng gói được ký ad hoc, chưa notarize: máy khác mở lần đầu phải chuột phải → Open.

## Kết nối

Mở app sẽ gặp màn hình **Kết nối**:
- **Thư mục local**: chọn một thư mục bất kỳ trên máy.
- **Server NAS**: nhập IP hoặc tên server (không hard code). App liệt kê các share account được phép truy cập (`smbutil view`), mount những share còn thiếu bằng mật khẩu trong Keychain (`osascript mount volume "smb://user@host/share"`), rồi quét tất cả share đã chọn vào chung một index. Các server đang mount sẵn hiện làm gợi ý.

Đây là cùng cơ chế với Cmd+K trong Finder: share SMB được mount vào `/Volumes/...` và app đọc nó như thư mục thường. Kerio VPN chỉ mở đường mạng tới IP nội bộ, nên khi đã bật Kerio thì nhập IP nào Kerio đi tới được cũng kết nối được. Nếu Keychain chưa có mật khẩu của server đó, hãy mount một share bằng Cmd+K một lần và chọn lưu mật khẩu.

Mỗi phiên dùng một nguồn (một thư mục local hoặc một server). Mỗi nguồn có file index riêng, nên đổi sang server khác không làm mất index của server cũ. Chưa tìm được trên hai server cùng lúc.

## Tìm ảnh

- Tìm chạy hoàn toàn trong RAM, không đi qua mạng. Đo với 3 triệu tên: khoảng 22 ms trên 2 core.
- So khớp theo **tên file** (không theo đường dẫn). Tên và từ khoá đều được đổi về chữ thường và bỏ dấu (`Ảnh Cá` → `anh ca`). Gõ nhiều từ cách nhau bằng dấu cách thì tên phải chứa đủ mọi từ, thứ tự nào cũng được.
- "Chỉ ảnh" giữ lại các đuôi jpg, jpeg, png, heic, tif, tiff, psd, webp, gif, cr2, cr3, nef, arw, dng.
- Bảng hiện tối đa 2.000 dòng; thẻ Search ghi tổng số kết quả và thời gian tìm.
- Mỗi dòng có tên, thư mục, dung lượng, ngày sửa. "Mở file" và "Show in Finder" mở file ngay trên ổ mạng.

## Index và cơ chế quét

Index nằm trong RAM dưới dạng các mảng phẳng (tên, thư mục cha, dung lượng, ngày sửa). Nó được lưu thành một file snapshot nhị phân trên máy (`~/Library/Application Support/ScanImage/index/`, không bao giờ ghi lên NAS), có checksum để phát hiện file hỏng. Nạp 3 triệu mục mất khoảng 0,35 giây.

### Lần quét đầu

1. App kiểm tra server có trả lời không rồi mount các share. Không tới được server thì dùng index đã lưu và không bật hộp thoại mount nào.
2. 48 worker liệt kê song song toàn bộ cây thư mục. Mỗi lần liệt kê dùng `getattrlistbulk`: một lệnh lấy cả tên, loại, dung lượng và ngày sửa của một thư mục, thay vì một `lstat` qua mạng cho mỗi file.
3. Kết quả vào index ngay, nên tìm được trong lúc đang quét.
4. Cứ 5 phút app lưu tạm index xuống đĩa; tắt app giữa chừng không mất phần đã quét.

### Các lần quét lại (incremental)

SMB không có change journal: server không cho biết thư mục nào vừa đổi. Thứ duy nhất app có là mtime của thư mục, và mtime chỉ đổi khi có thêm, xoá hoặc đổi tên **ngay trong** thư mục đó. Thêm file vào thư mục cháu không làm đổi mtime của thư mục ông.

App dựng index mới từ index cũ, đi từ gốc xuống. Thư mục gốc của share luôn được liệt kê. Mtime của từng thư mục con có sẵn trong kết quả liệt kê thư mục cha, nên việc so sánh không tốn thêm request nào:

| Thư mục con | App làm gì |
|---|---|
| Thư mục lá (không có thư mục con), mtime như cũ | Chép từ index cũ, không liệt kê |
| Có thư mục con, mtime như cũ, và 30 ngày qua không có gì bên trong thay đổi | Chép nguyên cả nhánh từ index cũ, không liệt kê thư mục nào bên trong |
| Có thư mục con, mtime như cũ, nhưng bên trong có thay đổi trong 30 ngày | Liệt kê, rồi xét tiếp từng thư mục con theo cùng luật |
| Mtime khác, hoặc thư mục mới | Liệt kê |

Hai trường hợp luôn bị liệt kê lại dù mtime như cũ:
- **Chưa đáng tin**: thư mục lần trước liệt kê lỗi, hoặc lúc được liệt kê mtime của nó mới hơn 10 phút (có thể đang bị ghi dở).
- **Tới lượt soát**: mỗi lượt quét, khoảng 1/64 số thư mục lẽ ra được bỏ qua vẫn bị liệt kê lại đầy đủ, kể cả mọi thứ bên dưới. Việc này bắt những thay đổi mà mtime không thấy: file bị ghi đè tại chỗ, hoặc thay đổi nằm sâu trong một nhánh cũ.

Hệ quả cần biết: thay đổi nằm sâu trong một nhánh đã yên 30 ngày có thể hiện trễ tới một vòng 64 lượt, khoảng 32 giờ khi quét mỗi 30 phút, lâu hơn nếu máy tắt đúng lượt đó. Khi phát hiện thay đổi, nhánh đó được quét mỗi lượt trở lại. "Quét lại toàn bộ" trong Settings → INDEX liệt kê mọi thư mục và thấy ngay.

Thẻ "Tốc độ quét" ghi số mục vừa liệt kê mỗi giây, số thư mục lỗi và số thư mục được bỏ qua trong lượt này.

### Khi nào app quét

- Khi mở app: nạp index đã lưu, rồi quét incremental ở nền.
- Mỗi 30 phút. Lượt trước chưa xong thì lượt này bị bỏ, không xếp hàng.
- 15 giây sau khi máy thức dậy.
- "Quét ngay" trên menu thanh menu (incremental), "Quét lại toàn bộ" trong Settings (liệt kê hết).

### Khi có sự cố

- **Một thư mục không đọc được**: giữ dữ liệu cũ của thư mục đó, đếm vào số lỗi, lượt sau thử lại. Dòng cảnh báo ghi các thư mục đầu tiên bị lỗi và lý do.
- **Mất mạng hoặc VPN**: app thử lại server; không trả lời thì dừng quét, giữ index cũ, báo offline.
- **Treo**: 5 phút không thư mục nào liệt kê xong thì app tự huỷ lượt quét.
- **Bấm Dừng**: phần chưa quét giữ dữ liệu cũ.
- Index mới thay index cũ trong một bước khi lượt quét kết thúc. Snapshot chỉ được ghi lại khi có gì thay đổi.

### Chạy nền

- Đóng cửa sổ là ẩn, app vẫn ở thanh menu (menu: Mở Scan Image, Quét ngay, Thoát). Cmd+Q mới thoát thật.
- **Mở cùng máy**: bật trong Settings → INDEX. macOS 13+ chỉ cho bật trên bản đã đóng gói (`go tool mygo build`), không bật được khi chạy `go tool mygo dev`.

### Tốc độ quét và giới hạn đã biết

- Đo ngày 13/10/2026 trên một share thật (64.496 thư mục, 2,9 triệu mục), ping tới NAS 8 ms, thư mục chưa cache: NAS trả được khoảng 4–10 lần liệt kê thư mục mỗi giây (120–330 mục/giây), và con số này không tăng khi chạy 4, 16 hay 48 worker; chỉ có độ trễ mỗi lần liệt kê tăng (p50 từ 0,28 giây lên 3,8 giây). Chưa biết nghẽn nằm ở NAS hay ở smbfs của macOS.
- Vì vậy thời gian một lượt quét lại phụ thuộc vào số thư mục phải liệt kê. Trên share đó: khoảng 19.950 lần liệt kê khi chỉ bỏ qua thư mục lá, còn khoảng 3.800–4.400 khi bỏ qua cả nhánh yên 30 ngày (tính bằng cách chạy lại lượt quét trên snapshot thật).
- Mỗi máy Mac quét riêng, nên nhiều máy cùng quét một NAS sẽ chia nhau tốc độ đó.
- Một số thư mục có tên tiếng Việt không mở được từ macOS (lỗi "no such file or directory" dù thư mục cha vẫn liệt kê ra tên đó). File bên trong không vào index. Nguyên nhân chưa xác định; nghi do dạng Unicode của tên lưu trên NAS.

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

## Phát triển

```bash
go test -race ./...                 # toàn bộ test
go test -race ./internal/...        # test lõi, không cần UI
go test -bench . ./internal/search  # benchmark tìm kiếm (mục tiêu: 3 triệu tên, p95 < 50 ms)
go run ./cmd/nasprobe -path /Volumes/<NAS> -limit 20000 -workers 8,32,64   # đo tốc độ liệt kê theo số worker
go run ./cmd/nasprobe -path /Volumes/<NAS> -lister readdir,bulk            # so lister thường với getattrlistbulk
```

`nasprobe -lister readdir,bulk` so lister `getattrlistbulk` của app (`source.LocalFS{Bulk: true}`) với `readdir` thường và in ra thư mục nào không đọc được. macOS cache kết quả liệt kê, nên đo lại trên cùng thư mục sẽ ra số cao hơn nhiều so với thư mục chưa cache. README trước đây ghi 2.000–4.000 mục/giây qua VPN; lần đo 13/10/2026 trên thư mục chưa cache thấp hơn nhiều (xem "Tốc độ quét và giới hạn đã biết"). Số đo trên đĩa local không có ý nghĩa vì page cache.

### Cấu trúc mã nguồn
```
app/                         package main của app (mygo.json: "main": "./app")
  main.go / app.go / view.go UI MyGo: kết nối, quét, cards, search, bảng kết quả, Settings xổ xuống
  tray.go                    icon thanh menu và menu chạy nền
  upload.go                  hàng đợi upload S3 (8 file song song, tiến độ, huỷ, thử lại)
  queue_view.go              tab Hàng đợi S3, nút copy tên file
  recheck.go                 kiểm lại file trên S3 sau khi đẩy, đẩy lại nếu bị ghi đè
  broken*.go                 tab File hỏng: danh sách, khớp tên, tìm theo ngày, đẩy
  dest*.go                   popup chọn thư mục đích S3
icons/                       icon Lucide (ISC) nhúng vào binary
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

## Đóng góp

Issue và pull request đều được hoan nghênh. Trước khi gửi PR:

- Chạy `gofmt`, `go vet ./...` và `go test -race ./...`.
- Thay đổi hành vi cần có test đi kèm.
- Giữ các luật kiến trúc:
  - `internal/*` không import MyGo; UI chỉ nằm trong `app/`.
  - Mọi truy cập file đi qua `source.Source`; thêm nguồn mới thì viết adapter, không đi vòng.
  - Độ trễ mạng là nút cổ chai: không stat hay đọc từng file một trên luồng UI.
  - Tìm kiếm chạy trong RAM trên các mảng phẳng của `internal/search`; không chuyển sang SQLite.
  - Index chỉ ghi thêm: quét lại là dựng index mới từ index cũ rồi thay thế, không sửa index đã công bố.

## Giấy phép

Dự án chưa có file `LICENSE`. Icon lấy từ [Lucide](https://lucide.dev) theo giấy phép ISC (`icons/LICENSE-lucide.txt`).
