# Index lưu trên máy + quét incremental chạy nền (kiểu Everything)

## Context

Hiện index chỉ nằm trong RAM (`internal/search`). Mỗi lần mở app, `app.startScan` tạo `search.New()` rồi quét lại toàn bộ NAS qua SMB/VPN; đóng app là mất hết. Với NAS cỡ TB qua Kerio VPN, lần quét này tốn hàng chục phút và chưa quét xong thì chưa search đủ.

Mục tiêu: hoạt động như Everything.

- Quét một lần, **lưu index trên máy từng người**. Mở app là nạp file và search ngay, kể cả khi chưa bật VPN.
- Index tự cập nhật ở nền bằng **quét incremental** viết bằng Go, không làm giật UI.
- App **ở lại chạy nền** sau khi đóng cửa sổ và tự mở khi đăng nhập máy.

Quyết định đã chốt với người dùng:

| Điểm | Chọn |
|---|---|
| Định dạng lưu | Binary snapshot của các arena (không dùng SQLite; plugin sqlite của MyGo import `mygo` nên không đặt được trong `internal/*`) |
| Chạy nền | Cùng một process: đóng cửa sổ = ẩn, icon menu bar, login item, quét mỗi 30 phút và khi máy thức dậy |
| Tốc độ liệt kê | Viết lister `getattrlistbulk`, đo bằng `nasprobe` trên NAS thật rồi mới quyết định bật |

SMB không có change journal như NTFS, nên "cập nhật" nghĩa là đi lại cây thư mục nhưng **bỏ qua những thư mục lá có mtime không đổi**. Trong kho ảnh, thư mục lá chiếm đa số thư mục và gần hết số file, nên phần lớn round trip được cắt.

Thiết kế dưới đây đã qua một vòng phản biện độc lập; các lỗ hổng tìm được đã đưa vào (đánh dấu ◆).

## Thiết kế

Nguyên tắc xuyên suốt: index vẫn **append-only**. Quét lại không sửa index cũ mà **dựng index mới rồi tráo** (rebuild + swap). Index cũ đứng yên nên search không tranh khoá với scanner, và lỗi mạng giữa chừng không làm hỏng dữ liệu đang có.

### 1. `internal/fold` và `internal/search`: cờ thư mục, Clone, snapshot

`fold.go`
- Thêm `Append(dst []byte, name []byte) []byte` không cấp phát: ASCII thường thì nối thẳng, ASCII có chữ hoa thì hạ từng byte, còn lại mới đi theo rune. `Bytes` hiện cấp phát một `strings.Builder` cho mọi tên có chữ hoa (`IMG_1234.JPG`), tức gần như mọi file máy ảnh; sửa chỗ này cũng làm nhanh luôn đường quét.

`index.go`
- Đổi `isDir []bool` thành `flags []uint8` với `flagDir` và `flagStable`.
- Bất biến của `flagStable` trên thư mục D: "các con đang lưu của D đúng bằng một lần liệt kê thật tại thời điểm L, và mtime M của D thoả `M < L − RacyWindow`" (`RacyWindow = 10 phút`). Cờ này chặn trường hợp thư mục đổi hai lần trong cùng một giây quanh lúc liệt kê. Thư mục chưa liệt kê, liệt kê lỗi, hoặc vừa mới đổi đều không có cờ và sẽ được liệt kê lại lần sau.
- Bất biến có sẵn, nay ghi thành doc và có test: con của một thư mục chiếm một dải id liên tiếp, và `parent[i] < i`.
- ◆ Root có định danh riêng: `AddRoot(key, path string)` với `key` là `smb://host/share` (hoặc đường dẫn tuyệt đối với nguồn local), `path` là mountpoint hiện tại. `SetRootPath(key, path)` cập nhật khi macOS mount lại ra `/Volumes/Photos-1`. Index cũ và mới khớp root theo `key`, không theo đường dẫn.
- `Clone() *Index`: bản sao copy-on-write O(1). Header slice được chụp dưới `ix.mu` thành view cắt cap (`s[:n:n]`); riêng `flags` và map roots được copy thật, vì `flags` là mảng duy nhất bị sửa tại chỗ. Nhờ vậy ghi file không giữ `RLock`, UI không khựng.

`snapshot.go` (mới)
- `Write(w io.Writer, ix *Index, m Meta) error`, `Read(r io.Reader, size int64) (*Index, Meta, error)`, `Meta{Complete bool; ScannedAt time.Time}`.
- Layout little-endian: `"SIDX"`, version, meta, bảng roots (`id`, `key`, `path`), `n`, rồi các section **có tag và độ dài** (`names`, `nameOff`, `parent`, `size`, `mtime`, `flags`), cuối file là CRC32C. Reader bỏ qua tag lạ, nên thêm section sau này không buộc mọi máy quét lại. Mã hoá theo khối bằng `encoding/binary`, không dùng `unsafe`.
- ◆ **Không lưu `folded`/`foldOff`**: chúng chiếm 38% file và nếu bảng fold đổi thì query sẽ lệch âm thầm. Khi nạp, fold lại song song theo `GOMAXPROCS` (khoảng 0,1 giây cho 3 triệu tên). File còn khoảng 165 MB cho 3 triệu mục.
- `Read` không tin file: kiểm magic, version; ◆ mỗi độ dài section phải nhỏ hơn phần còn lại của file **trước khi cấp phát**; sau đó kiểm CRC, độ dài khớp `n`, offset tăng dần và kết thúc đúng độ dài arena, `parent[i] < i`, id của root hợp lệ. File hỏng trả lỗi, không bao giờ panic. `nFiles`, `nDirs`, `byteSum` tính lại khi nạp.
- Sai version hoặc CRC: bỏ snapshot, quét lại từ đầu.

### 2. `internal/search/builder.go` (mới): dựng index mới từ index cũ

```go
const NoPrev = ^uint32(0)

func NewBuilder(old *Index, opt BuildOptions) *Builder // old có thể nil
func (b *Builder) AddRoot(key, path string) (id, prev uint32)
func (b *Builder) Listed(dir, prev uint32, entries []source.Entry) []Child
func (b *Builder) Failed(dir, prev uint32)
func (b *Builder) Index() *Index
func (b *Builder) Changed() bool
func (b *Builder) Stats() BuildStats // ReusedDirs, ReusedEntries, KeptDirs
```

`BuildOptions{Reuse bool; VerifySlot, VerifySlots uint32; Now func() time.Time}`.

- `Listed`: nối `entries` dưới `dir`; đặt `flagStable` cho `dir` nếu mtime của nó cũ hơn `now − RacyWindow`. Với mỗi thư mục con, tìm bản cũ theo tên trong dải con của `prev`. **Tái dùng** (copy khối byte từ arena cũ sang, không gọi mạng, không đi xuống) khi cả bốn điều đúng: bản cũ có `flagStable`; mtime cũ bằng mtime vừa liệt kê; bản cũ **không có thư mục con**; và thư mục này không tới lượt kiểm tra lại (xem dưới). Còn lại trả về để scanner liệt kê.
- Vì sao chỉ thư mục lá: thư mục có con thì phải liệt kê, vì đó là cách rẻ nhất (một round trip) để biết mtime hiện tại của mọi thư mục con.
- `Failed`: nếu có bản cũ thì copy nguyên cây con cũ sang (từng thư mục một khối, giữ cờ của hậu duệ), còn chính `dir` để không có `flagStable` để lần sau liệt kê lại. Dùng cho ba trường hợp: `ReadDir` lỗi, share chưa mount, và job còn dang dở khi quét bị huỷ.
- ◆ `Listed` và `Failed` mỗi lần là **một critical section** trên index mới, nên một `Clone()` không bao giờ thấy "có cờ stable nhưng chưa có con".
- ◆ **Kiểm tra lại xoay vòng** thay cho chế độ "quét full hàng tuần" (chế độ đó có thể không bao giờ chạy xong qua VPN chập chờn): mỗi lượt, thư mục lá nào có `hash(đường dẫn) % 64 == VerifySlot` bị liệt kê lại bất kể mtime. `VerifySlot` tính từ đồng hồ (`unix / chu kỳ quét % 64`), không cần lưu trạng thái. Mỗi lượt tốn thêm khoảng 1,5% số lá; sau khoảng 64 lượt mọi thư mục đều đã được soát. Cơ chế này tự chữa những gì mtime không thấy: file bị ghi đè tại chỗ, filesystem không cập nhật mtime thư mục, thư mục bị thay bằng thư mục khác trùng tên trùng mtime, đồng hồ NAS lệch.
- Chặn mountpoint rỗng: root liệt kê ra 0 mục trong khi bản cũ có con thì coi là `Failed`.
- ◆ `Changed()` là false khi mọi thư mục đã liệt kê có danh sách con và cờ giống hệt bản cũ, không có root thêm hay bớt. Khi đó **không tráo, không ghi file**: thứ tự kết quả trên màn hình không nhảy, và không ghi 165 MB xuống SSD 48 lần mỗi ngày.
- Chế độ full (`Reuse=false`, nút "Quét lại toàn bộ") liệt kê tất cả nhưng vẫn giữ hành vi `Failed`.

Hệ quả: scanner dừng vì lý do gì (xong, bị huỷ, mất mạng) thì index mới cũng **không kém hơn** index cũ.

### 3. `internal/scanner` và `internal/source`: sink biết bỏ qua, biết lỗi, huỷ là dừng ngay

```go
type Dir struct { Path string; ID, Prev uint32 }
type Sink interface {
    Listed(d Dir, entries []source.Entry) []Dir // các thư mục con còn phải liệt kê
    Failed(d Dir, err error)
}
func (s *Scanner) Run(ctx context.Context, roots []Dir, sink Sink) error
```

- Hợp đồng: mỗi `Dir` đưa vào scanner kết thúc bằng đúng một lần `Listed` hoặc `Failed`.
- ◆ Huỷ là dừng ngay. `os.ReadDir` không huỷ được và có thể treo 30 giây trở lên trên mount SMB chết, nên `Run` không chờ `wg.Wait()` như hiện nay. Khi `ctx` kết thúc, điều phối viên đặt cờ `closed` dưới mutex của sink, gọi `Failed` cho mọi job đang xếp hàng **và đang chạy dở**, rồi trả về. Worker trả kết quả muộn thấy `closed` thì bỏ. Nhờ vậy nút Dừng, thoát app và mất mạng đều không phải chờ timeout, và không có kết quả muộn nào ghi vào index đã công bố.
- Giữ nguyên worker pool, hàng đợi LIFO và việc tuần tự hoá lời gọi sink. `cmd/nasprobe` đổi sang interface mới.
- ◆ `source/localfs.go`: hiện `de.Info()` lỗi thì bỏ qua mục đó và vẫn báo thành công. Khi VPN chập chờn giữa một thư mục, danh sách thiếu sẽ bị coi là thật. Sửa: lỗi khác `fs.ErrNotExist` làm cả thư mục trả lỗi (đi đường `Failed`); chỉ `ErrNotExist` (file vừa bị xoá) mới được bỏ qua.

### 4. `internal/indexer` (mới): điều phối, không phụ thuộc UI

Một `Service` cho một nguồn. Đổi nguồn là `Close()` cái cũ và mở cái mới, nên một lượt quét kết thúc muộn không bao giờ ghi nhầm vào file của nguồn khác.

`store.go`
- Thư mục `os.UserConfigDir()/ScanImage/index/` (tức `~/Library/Application Support/ScanImage/index/`), cùng quy ước với `s3up.ConfigPath`. Không bao giờ ghi lên NAS.
- `Save` ghi qua `os.CreateTemp` trong cùng thư mục, `Sync`, rồi `rename` thành `<sha1(source)>.sidx`. Các lần lưu được tuần tự hoá. `Load` trả `os.ErrNotExist` khi chưa có.
- ◆ Lần đầu tạo thư mục, gọi `tmutil addexclusion` (best effort) để Time Machine không sao lưu lại 165 MB mỗi lần index đổi.
- `session.json`: nguồn mở lần cuối (`Mode`, `Source`, `Host`, `User`, danh sách `{Key, Share}`).

`service.go`
- `Open(store, session, opts)`: nạp snapshot, `Current()` search được ngay.
- `Scan(mode)`: chạy `Prepare`, tạo `Builder`, chạy `Scanner`. Khi `Run` trả về: nếu `Changed()` thì tráo `Current()`, tăng `Gen` và lưu; nếu không thì chỉ cập nhật thời điểm kiểm tra trong RAM.
- Index nào được search trong lúc quét: có index cũ thì dùng index cũ cho tới khi lượt quét kết thúc; chưa có (lần quét đầu tiên) thì dùng index mới ngay để kết quả tăng dần như hiện tại.
- Checkpoint: trong lần quét đầu tiên, lưu `Clone()` mỗi 5 phút. Crash hay mất điện chỉ mất tối đa 5 phút, và lần mở sau quét tiếp từ đó (thư mục chưa có cờ stable thì được liệt kê, lá đã ổn định thì tái dùng).
- Lịch: timer 30 phút và `Kick(delay)` cho sự kiện thức dậy. Không chạy chồng hai lượt quét.
- `Stop()` huỷ lượt quét; vì scanner dừng ngay nên phần hoàn tất (`Failed` cho job dở, tráo, lưu) xong trong vài giây. `Close()` là `Stop()` rồi chờ có giới hạn 5 giây; không có đường xử lý riêng cho lúc thoát.
- ◆ Mất mạng giữa lượt quét SMB: ở lỗi mạng đầu tiên của `ReadDir`, kiểm tra lại `netshare.Reachable` (có debounce). Không tới được thì huỷ lượt quét và đặt trạng thái offline. Không dùng bộ đếm lỗi liên tiếp.
- `State()` trả ảnh chụp cho UI: `Scanning`, `Offline`, `Progress`, `BuildStats`, `CheckedAt`, `Complete`, `Gen`, danh sách root `{Name, Path}`. `Notify func()` báo UI có thay đổi.

`prepare.go`
- Nguồn local: dùng thẳng thư mục; root không đọc được thì `Failed` lo.
- Nguồn SMB: `netshare.Reachable(host)` (TCP dial cổng 445, timeout 3 giây). Không tới được thì offline và bỏ lượt, không gọi `osascript` nên không bật hộp thoại nào khi VPN tắt. Tới được thì `Discover` + `Ensure` cho các share đã lưu, lấy mountpoint hiện tại từ `mount` cho đúng `host/share` rồi `SetRootPath`. Share nào không mount được thì root đó đi đường `Failed`.
- ◆ Mount ở nền thất bại một lần thì không thử lại ở nền nữa cho share đó, cho tới khi người dùng bấm "Quét lại" hoặc mở lại app. Tránh hộp thoại hỏi mật khẩu lặp lại mỗi 30 phút.

### 5. Package `main`: UI và chạy nền

`app.go`
- Bỏ các field `index`, `scan`, `progress`, `cancel`, `scanning`, `scanGen`, `roots`; thay bằng `svc *indexer.Service` và `st indexer.State`.
- `startScan`/`stopScan` thành `indexer.Open` + `svc.Scan` / `svc.Stop`. `refreshSearch` dùng `svc.Current()`, chỉ chạy lại khi `Gen` hoặc query đổi.
- ◆ `selected` là chỉ số trong `items`, mà sau mỗi lần tráo id bị xếp lại. Trước khi làm mới, nhớ đường dẫn của dòng đang chọn rồi tìm lại vị trí theo đường dẫn (không thấy thì bỏ chọn), để "Mở file" và "Upload S3" không trúng nhầm file. Kết quả hiển thị được sắp theo đường dẫn để thứ tự ổn định qua các lần tráo.
- `resume()`: lúc khởi động, có `session.json` thì vào thẳng màn hình browse, nạp snapshot ở goroutine, rồi `svc.Scan(Incremental)`.
- `disconnect()` đóng service và xoá session (file snapshot giữ lại, kết nối lại nguồn cũ vẫn nạp tức thì).

`upload.go`: `relKey` dùng tên root (tên share) thay cho `filepath.Base(mountpoint)`, để key S3 không đổi thành `Photos-1/...` khi macOS mount lại ra đường dẫn khác.

`main.go`
- `RequestSingleInstanceLock` + `OnSecondInstance` (login item và mở tay không tạo hai process cùng quét).
- Cửa sổ tạo với `Hidden: mygo.App.WasOpenedAtLogin()`.
- `win.OnClose`: nếu không đang thoát thì `e.PreventDefault()` rồi `win.Hide()`. Cờ đang thoát đặt trong `App.OnBeforeQuit` (MyGo gọi hook này trước khi đóng các cửa sổ). Cửa sổ không bị huỷ nên `win.Update` vẫn chạy, trạng thái upload và thumbnail không bị rơi.
- `App.OnActivate` và tray hiện lại cửa sổ. `App.OnWillQuit` gọi `svc.Close()`. `mygo.Power.OnResume` gọi `svc.Kick(15s)`.

`tray.go` (mới): `mygo.NewTray` với icon template 32×32 vẽ bằng `image/png` trong code; menu "Mở Scan Image", "Quét ngay", "Thoát".

`view.go`
- Header: "Quét lại" chạy incremental. Dòng trạng thái hiện "Kiểm tra lúc …", "Đang cập nhật…", "Index chưa đầy đủ, đang quét tiếp", "Offline, đang dùng index đã lưu".
- Card "Tốc độ quét" thêm số thư mục được bỏ qua.
- Settings thêm mục **INDEX**: checkbox "Mở cùng máy và chạy nền" (`App.SetOpenAtLogin`, báo lỗi rõ khi chạy `mygo dev` vì macOS 13+ cần app đã đóng gói), nút "Quét lại toàn bộ", đường dẫn và dung lượng file index.

### 6. Lister `getattrlistbulk` (đo trước, mặc định tắt)

- `internal/source/bulk_darwin.go`: `LocalFS.Bulk bool`; khi bật, `ReadDir` lấy tên + loại + size + mtime theo lô. File stub cho hệ khác.
- Cách gọi đã kiểm trong toolchain: `purego.Dlsym(purego.RTLD_DEFAULT, "getattrlistbulk")` rồi `purego.SyscallN`, chỉ tin `errno` khi `int32(r1) == -1`. `purego` v0.11.1 đã có trong build (chuyển từ indirect sang direct trong `go.mod`). Không dùng trap thô `SYS_GETATTRLISTBULK` (x/sys đánh dấu deprecated) và không dùng `RegisterLibFunc` (không trả được errno).
- Điểm dễ sai khi parse buffer: phải xin `ATTR_CMN_RETURNED_ATTRS` và `ATTR_CMN_NAME`; đọc theo bitmap **trả về**, không theo bitmap đã xin (thư mục không có nhóm thuộc tính file); `ATTR_CMN_ERROR` nằm ngay sau bitmap trả về và khác 0 thì cả thư mục trả lỗi; offset của tên tính từ chính `attrreference`, độ dài gồm cả NUL; bản ghi căn 8 nhưng field đóng gói 4, nên giải mã theo offset, nhảy theo `length` và kiểm biên cả hai; trả 0 là hết, `ERANGE` là tăng buffer; symlink (`VLNK`) không phải thư mục.
- `cmd/nasprobe`: thêm cờ `-lister readdir,bulk` để in hai dòng so sánh.
- App giữ `Bulk: false`. Người dùng chạy `nasprobe` trên NAS qua VPN; nếu nhanh hơn rõ thì bật bằng một dòng ở bước sau.

## Thứ tự thực hiện

Mỗi bước theo TDD (test đỏ trước), chạy `go test -race ./internal/...`.

1. **T1** `fold.Append`; `search`: `flags`, root có `key`, `Clone`, snapshot `Write`/`Read` (fold lại khi nạp), fuzz `Read`, benchmark nạp/ghi 3M.
2. **T2** `search`: `Builder` (tái dùng, `Failed`, kiểm tra xoay vòng, `Changed`, chặn root rỗng, chế độ full).
3. **T3** `scanner`: interface `Sink`, `Failed`, huỷ dừng ngay; `LocalFS` hết nuốt lỗi `Info()`; cập nhật `nasprobe`.
4. **T4** `indexer`: `Store` (ghi atomic, session, loại trừ Time Machine).
5. **T5** `indexer`: `Service` + `prepare` + `netshare.Reachable`.
6. **T6** `main`: chuyển `app.go`/`view.go`/`upload.go` sang `Service`, `resume()` lúc khởi động, giữ lựa chọn theo đường dẫn. Từ đây app đã "mở là search ngay".
7. **T7** `main`: ẩn khi đóng, tray, login item, thức dậy, lưu khi thoát, single instance.
8. **T8** `source`: lister bulk + cờ `nasprobe`.
9. **T9** tài liệu: `README.md` (P1 hoàn thành, cách chạy nền), `CLAUDE.md` (dòng SQLite đổi thành "persistence là binary snapshot ở `internal/search` + `internal/indexer`, trên đĩa local, không bao giờ trên NAS").

T1 đến T5 không đụng UI. T3 độc lập với T1/T2; T4 cần T1; T5 cần cả bốn. Trước khi báo xong: `gofmt`, `go vet ./...`, rồi review bằng `go-reviewer` và `silent-failure-hunter`. Chỉ commit khi được yêu cầu.

## File chạm tới

| File | Thay đổi |
|---|---|
| `internal/fold/fold.go` | `Append` không cấp phát |
| `internal/search/index.go` | `flags`, root có `key`, `Clone`, doc bất biến |
| `internal/search/snapshot.go`, `builder.go` | mới |
| `internal/scanner/scanner.go` | interface `Sink`, `Failed`, huỷ dừng ngay |
| `internal/source/localfs.go`, `bulk_darwin.go`, `bulk_other.go` | lỗi `Info()` làm hỏng cả thư mục; cờ `Bulk`, lister mới |
| `internal/indexer/{store,service,prepare}.go` | mới |
| `internal/netshare/netshare.go` | `Reachable` |
| `cmd/nasprobe/main.go` | sink mới, cờ `-lister` |
| `app.go`, `view.go`, `upload.go`, `main.go`, `tray.go` | dùng `Service`, chạy nền |
| `go.mod` | `purego` thành direct |
| `README.md`, `CLAUDE.md` | cập nhật |

Tái dùng code có sẵn: `source.Source`/`LocalFS`, worker pool của `scanner.Scanner`, `netshare.Client` (`Discover`, `Ensure`, `Mounted`, seam `Runner` cho test), quy ước thư mục của `s3up.ConfigPath`, `app.update`.

## Kiểm chứng

Tự động
- `go test -race ./internal/...`, `go vet ./...`, `gofmt -l .` sạch.
- Snapshot: round trip cho kết quả search, `Item`, `Stats`, roots giống hệt; file cắt cụt, lật một byte, sai magic, độ dài section khổng lồ đều trả lỗi mà không cấp phát lớn; `FuzzRead` không panic; ghi `Clone()` trong khi goroutine khác đang `AddChildren` sạch dưới `-race`.
- Builder (bảng test trên cây giả): lá không đổi được tái dùng; mtime đổi thì liệt kê lại; thư mục vừa đổi không được `flagStable`; lá tới lượt `VerifySlot` bị liệt kê lại dù mtime không đổi; thư mục và file bị xoá biến mất, mục mới xuất hiện; `Failed` giữ cây cũ và lần sau liệt kê lại; root rỗng không xoá sạch index; root đổi mountpoint vẫn khớp theo `key`; không đổi gì thì `Changed()` là false.
- Scanner: với `Source` giả bị treo, `Run` trả về ngay sau khi huỷ và mỗi `Dir` nhận đúng một `Listed` hoặc `Failed`; kết quả muộn bị bỏ.
- `LocalFS`: lỗi `Info()` khác `ErrNotExist` làm `ReadDir` trả lỗi.
- Indexer với `Source` giả đếm số lần `ReadDir`: quét đầu rồi mở `Service` mới thì search được với 0 lần `ReadDir`; sau khi sửa cây, số `ReadDir` đúng bằng số thư mục không phải lá cộng số lá đã đổi cộng số lá tới lượt soát; không đổi gì thì file snapshot không bị ghi lại; nguồn mất kết nối thì index và file nguyên vẹn; ghi lỗi giữa chừng không làm hỏng file cũ.
- Benchmark: `go test -bench . ./internal/search` giữ search 3M tên p95 < 50 ms; nạp snapshot 3M mục < 1 giây.
- Lister bulk: kết quả trùng `os.ReadDir` + `Info()` trên cây tạm có tên Unicode NFD, symlink, thư mục rỗng, và thư mục đủ lớn để phải gọi nhiều lần.

Thủ công (`go tool mygo dev`)
1. Quét một thư mục local lớn, thoát, mở lại: vào thẳng màn hình browse, search được trước khi quét lại xong.
2. Thêm, xoá, đổi tên vài file và thư mục, bấm "Quét lại": kết quả đúng, card báo số thư mục được bỏ qua, dòng đang chọn vẫn là đúng file đó.
3. Nguồn NAS: tắt VPN rồi mở app, vẫn search được, trạng thái "Offline", không có hộp thoại nào bật lên. Bật VPN lại, lượt quét kế tiếp tự chạy.
4. Rút mạng giữa lúc quét lại: lượt quét dừng trong vài giây, index không mất mục nào.
5. Đóng cửa sổ: app còn ở menu bar, "Mở Scan Image" hiện lại ngay; Cmd+Q thoát thật trong vòng 5 giây.
6. Bản đóng gói (`go tool mygo build`): bật "Mở cùng máy", đăng nhập lại, app khởi động ẩn.
7. `go run ./cmd/nasprobe -path /Volumes/<NAS> -lister readdir,bulk` trên NAS thật để quyết định bật lister bulk.

## Giới hạn đã biết và ngoài phạm vi

- File bị ghi đè tại chỗ (thư mục cha không đổi mtime) giữ size/mtime cũ cho tới khi thư mục đó tới lượt soát xoay vòng (tối đa khoảng 64 lượt quét) hoặc khi bấm "Quét lại toàn bộ".
- Lần quét đầu bị dở rồi quét tiếp: màn hình giữ phần đã có cho tới khi lượt quét tiếp kết thúc hoặc bị dừng, không tăng dần từng giây như lần quét đầu.
- Trong lúc quét lại, RAM tạm thời chứa hai index (khoảng 2 × 250 MB với 3 triệu mục).
- So `RacyWindow` dùng đồng hồ NAS với đồng hồ Mac. Nếu NAS chậm hơn 10 phút, cửa sổ an toàn hẹp lại; soát xoay vòng là lưới đỡ.
- Không làm đợt này: giao diện chỉnh chu kỳ quét, dọn snapshot của nguồn cũ, ẩn icon Dock khi chạy nền, định danh thư mục bằng inode (inode của smbfs có thể đổi sau mỗi lần mount lại, làm mất hết lợi ích incremental), đóng gói `.dmg` và notarize (P5).
