# STATUS — persistent-index
phase: review — code done and verified, awaiting manual app test and commit decision
branch: feature/persistent-index
base: main
updated: 2026-10-13

## Gates
- [ ] Gate 1 — spec approved (no spec.md; plan.md was written directly from the agreed design)
- [x] Gate 2 — plan approved (user confirmed in chat, 2026-10-13)
- [ ] Gate 3 — review approved
- [ ] Final — shipped (MR: <link>)

## Task log
<!-- implementer appends: Tn done — files — tests -->
T1 done — internal/fold/fold.go, internal/search/index.go, internal/search/snapshot.go — tests 30/30
T3 done — internal/scanner/scanner.go, internal/source/localfs.go, cmd/nasprobe/main.go — tests 8/8
T8 done — internal/source/localfs.go, internal/source/bulk_darwin.go, internal/source/bulk_other.go, cmd/nasprobe/main.go, go.mod — tests 12/12 internal/source (bulk + localfs), internal/scanner green
T2 done — internal/search/builder.go, internal/search/builder_copy.go, internal/search/builder_bench_test.go — tests 77/77 internal/search (race)
T5 done — internal/indexer/{service,scan,prepare}.go, internal/netshare/netshare.go (Reachable), internal/indexer/service_test.go (1 test fixed) — tests internal/indexer + internal/netshare green with -race, indexer 10x green
T6 done — app.go, view.go, upload.go, main.go (one-line a.resume()) — tests 18/18 package main (3 tests + 15 subtests, race); build/vet/gofmt clean
review fixes A–K done — internal/indexer/{scan,service,prepare,store}.go, internal/search/{builder,snapshot,index}.go, internal/source/{localfs,bulk_darwin}.go + tests (review_test.go new; service_test.go, localfs_test.go adjusted) — tests 180/180 top-level (race), indexer 10x green

## Decisions / deviations
- No per-task commits: plan.md says commit only when the user asks.
- Package `main` (root `*.go`) is allowed to not compile between T1/T3 and T6; T6 migrates it. Until then verify with `go test -race ./internal/...` and `CGO_ENABLED=0 go vet ./internal/... ./cmd/...`.
- This machine has no Xcode command line tools: always pass `CGO_ENABLED=0` to `go vet` / `go build`.
- Wave order: (T1 ‖ T3) → (T2 ‖ T4 ‖ T8) → T5 → T6 → T7 → T9.
- purego imports runtime/cgo when cgo is on, so every package that imports internal/source needs `CGO_ENABLED=0` here (no Xcode CLT). Working commands: `CGO_ENABLED=0 go test -count=1 ./internal/...` and `CGO_ENABLED=0 go test -race -count=1 ./internal/...` (race works without cgo on darwin/arm64 with this toolchain), `CGO_ENABLED=0 go vet ./internal/... ./cmd/...`.
- T8: non-darwin `Bulk` is ignored and readdir is used (bulk_other.go keeps a stub returning ErrUnsupported that is never reached).
- T8: directory `Size` is 0 in both listers (`source.Entry.Size` doc says so); bulk requests no size attribute for directories, so the listers agree on any filesystem, SMB included.
- T4 done — internal/indexer/store.go — tests 19/19
- T5: `Current()` during the FIRST scan is the live growing index; `Gen` is bumped when it becomes current and again at the swap, but it does NOT change as entries are added. T6 must refresh results from `State().Progress` ticks (or Notify) during a first scan, not only on `Gen`.
- T5: `Complete` = scan ran to the end, not offline, and at least one directory listed. Directories that fail individually keep old data and count in `Progress.Errors`; they do not clear Complete.
- T5: `CheckedAt` moves only when a scan ran to the end. A cancelled or offline scan leaves it alone.
- T5: prepare uses `Client.Mounted` + `MountShare` per saved share instead of `Discover` + `Ensure`: it skips the 20 s `smbutil view` round trip and only needs the mountpoint. A share that cannot be mounted and has no root in the old index gets no root at all (nothing to keep).
- T5: State has extra fields `SaveErr` (last snapshot write failure; in-memory swap kept; the next scan retries the save even if nothing changed), `ScanErr` (unmountable shares / prepare failure of the last scan) and `LoadErr` (snapshot existed but was unusable).
- T5 test fix: TestStopDuringRescanLeavesIndexNoWorseThanBefore expected entries the edit had deleted (b/b2) to survive a cancelled rescan; whether they do depends on whether `b` was listed before the cancel. It now requires only paths that still exist in the source.
- T6: app methods for T7 (all UI-thread state, but the first three are safe from any goroutine because they go through a.update): `a.resume()` (already hooked), `a.scanNow()` (tray "Quét ngay"), `a.kick(15*time.Second)` (mygo.Power.OnResume), `a.closeOnQuit()` (OnWillQuit; closes the service synchronously, up to 5 s). "Show window" needs no app method: call `a.win.Show()` directly. The window is never destroyed on close-to-hide, so `a.update` keeps working.
- T6: stale-service guard is `a.sessGen`, bumped by every openSession/closeService/closeOnQuit; Notify and ticker closures capture it and drop their update when it differs. A service opened for an outdated generation is closed in a goroutine.
- T6: `a.status` is now only a transient action message (upload queued etc.); it is cleared when Scanning flips or CheckedAt moves. The scan line (CheckedAt / Đang cập nhật… / offline / errors) is computed by `a.statusLine()`.
- T6: settings has `indexSection(c)` — T7 puts the "Mở cùng máy và chạy nền" checkbox there.
- T6: if the store directory cannot be created the connect screen shows the error and connect is refused (no in-memory fallback: indexer.Service requires a Store).
- T7 done — main.go tray.go tray_test.go view.go app.go — tests 4/root pkg (all pass)
- UI review fixes 1–5 done — app.go view.go tray.go app_test.go — tests 40/40 (root pkg, incl. subtests)
- Review fixes: new sentinels `indexer.ErrRootNotChecked` (wrapped in State().ScanErr, names the roots; CheckedAt does not move) and `indexer.ErrScanStalled`; builder guard exposed as `search.Builder.Rejected(dir)`.
- Review fixes: `Options.StallTimeout` (0 → 5 min, negative → off) cancels a scan with no Listed/Failed for that long; prepare's `Mounted` lookup has a 10 s timeout.
- Review fixes: pending-request rule — Scan/Kick during a scan keep ONE request (Full beats Incremental, user beats background and keeps the mount-retry privilege), started atomically when the scan ends; Stop and Close clear/ignore it; the periodic tick never queues.
UI batch (Bulk lister, recent-rate card, grid/thumb removal, tabs scaffold) done — app.go view.go broken.go app_test.go README.md, thumbs.go + internal/thumb deleted — tests pass
"File hỏng" tab done — internal/brokenlist (new), internal/s3up (UploadWith, Config.ListURL), internal/search/findnames.go, broken.go, upload.go — whole repo green with -race at that point
State.FailedDirs done — internal/indexer/{scan,service}.go, app.go `problems` — whole repo green with -race at that point

## Current state (2026-10-13, afternoon)

Nothing is committed (commit only when the user says so). Verify = `gofmt -l .`, `CGO_ENABLED=0 go vet ./...`, `CGO_ENABLED=0 go test -race -count=1 ./...`; last run green after everything listed under "Fixed" below.

Built and green: persistent index (T1–T9), bulk lister on by default, tabs "Tìm ảnh" / "File hỏng", list fetch + match by name, find by date (`internal/datefind`), dry run, push with overwrite + verify, report to `POST /api/v2/s3/missing-design-files/uploaded` (never send a real POST in development), S3 destination popup. The earlier "unverified lead edits" (datefind single-number day folders and root pruning, `matchRow`, `Folders` partial result on deadline) compiled and passed as written.

Fixed from the security review and review round 2 (each with a test written first):
- Symlinks: `s3up.UploadWith` opens through `openRegular` (Lstat, regular files only, `os.SameFile` after open); `sourceStamp.check` uses Lstat and refuses anything that is not a regular file.
- Default-layout push ("Theo cấu trúc share") now goes through the same Head check, confirmation step and source stamp as a folder push (`pushChecked`); it keeps the settings' options, and the confirmation says so when "Bỏ qua file đã có" is on. The Head budget grows with the number of keys (`headPerKey`). The popup title always names the bucket; the confirmation step names the bucket of the snapshot.
- `brokenlist.Key` refuses keys over 1,024 bytes and any Cc / Cf / Zl / Zp character; `Parse` keeps at most 1,000 rejected lines plus a count of the rest.
- `oneLine` strips control and format characters; `Fetch` / `Report` cap the echoed status text.
- s3up clients share one transport with a 2-minute response header timeout.
- `datefind.ParseDate` accepts runs of six to eight digits: DDMMYY plus a sequence number (`M15-05092610.png`), or DDMMYYYY.
- `Folders` deadline behaviour has a test (`TestFoldersDeadlineReturnsWhatWasFound`).

- HIGH, planted newer file: a row with more than one exact candidate is not auto-eligible. It shows "Có N bản, cần xác nhận" and needs the per-row "Dùng file này" for the path shown (`wantsConfirm`, `needsConfirm`, `confirmSource`); a newer copy appearing later drops the confirmation (`settleConfirm`). Dry run and the confirm dialog count these rows separately (`multiCount`).
- Memory: `findCandidates` looks each distinct name up once and keeps the newest 50 exact and 50 look-alike hits (`maxCandidates`, `splitCapped`).

Added after the review fixes (user asked 2026-10-13):
- Finished upload rows have a button that opens the object's address in the browser (`jobWebURL`, `openWeb`, `s3up.Client.ObjectURL`).
- Unattended mode for the File hỏng tab (`broken_auto.go`, `Config.AutoPush`, checkbox "Tự động (5 phút)" with a one-time confirmation naming the bucket). The user approved it in words: "cứ quét định kì rồi đẩy cập nhật lại product là được". Every `autoEvery` (5 min): fetch → match → find by date → `pushRows` → report. It pushes only rows that are eligible without a person (one exact copy, or a source the user confirmed), waits for `State.Complete`, does not start while uploads are running or unreported (`rowsBusy`), and never re-sends the same source for a key the server keeps listing (`autoRows`, `pushedSource`). Pure rules are tested in `broken_auto_test.go`; the phase chain (`autoTick` → `autoAfterFetch` → `autoAfterMatch` → `autoAfterQuick` → `autoPush`) has no test and has not been run in the app.
- List source (user asked 2026-10-13): the default list address is now the API `https://production.trackingis.info/api/queue-s3-upload`, a JSON array of `{"sku", "path"}` (`brokenlist.DefaultURL`, `parseQueue`). `Parse` still reads the old text form `File<TAB>Path`, so `missing_s3_files.txt` works when its address is typed into Settings. Both forms go through the same validation and caps (`collector`). One GET of each address on 2026-10-13 showed two different lists: 26 entries in the API, 25 in the text file, 9 in both.
- View changes (user asked 2026-10-13): the upload list moved from the panel under every tab to its own tab "Hàng đợi S3" (`queue_view.go`, `tabQueue`, `queueTabLabel`), a table of all jobs, newest first, with a "Đẩy lúc" column (`uploadJob.Done`, set by `finishJob` for done, skipped and failed runs; `jobTime`). The File hỏng table has the same column. File names in both tables show a copy button while the pointer is over the cell (`nameCell`, `c.WriteClipboard`). An upload from Tìm ảnh switches to the queue tab; a push from File hỏng and the unattended cycle do not. `uploadPanel` and `uploadsOpen` are gone. These are covered by headless view tests (`view_test.go`, `ui.NewTester`), and the two tabs were rendered to PNG once to check the layout; nobody has clicked through them in the running app. The job list lives in memory only.
- Report for uploads from Tìm ảnh (user asked 2026-10-13): `enqueueDest` hands each queued job to the File hỏng row that names its key (`adoptJobs`, `adoptUploads`), so the existing report flow tells the server once the upload is done, and the unattended cycle does not send the same source again. A key that is not in the fetched list is not reported: the endpoint marks listed files as uploaded, and what it does with an unknown file is not known. The list must have been fetched before the upload is queued. An upload that ends as "Đã có, bỏ qua" is not reported.
- Selected table rows (user said the color was hard to read): the row background is a darker blue (`colSelect`, `calmRows` swaps the accent while the table is built) and cells drop their muted or status color on the selected row (`tint`). Checked on a rendered PNG.
- Empty objects (user asked 2026-10-13, after the incident below): three guards. `s3up.UploadWith` refuses a 0-byte source. A File hỏng row whose source is 0 bytes is not eligible and says so (`eligibleRows`, `brokenStatus`). After a push the app looks at the object again 1 and 10 minutes later (`recheck.go`: `recheckDelays`, `recheck`, `headJobs`, `settleRecheck`, `uploadJob.Checks`/`Repush`); a changed size or a missing object queues the same job again, at most `maxRepush` = 3 times, then the job fails with a message naming the other writer. `clearFinished` keeps jobs still to be checked. Pending looks are lost when the app quits. Tested in `recheck_test.go` against an httptest bucket; the timer chain (`recheckWake`) has not run in the app.
- View changes, second round (user asked 2026-10-13): the File hỏng tab label counts the listed files (`brokenTabLabel`); every table starts with a fixed "#" column (`numberColumn`, `rowNumber`); the Tìm ảnh table has a tick box per row and a "Chọn tất cả" box beside the search field, both on the selection the upload already used (`chooseBox`, `allChosen`, `chooseAll`). Uploads run 8 at a time (`uploadWorkers`, was 3). Covered by headless view tests and one rendered PNG.

Still open:
- Incident 2026-10-13, cause not proven: five objects the app had pushed and verified at 15:59:16–15:59:26 (Vietnam time) were replaced by 0-byte objects, all at 15:59:42: `Etsy/Etsy-Chau/U-121026A30.png`, `A31`, `A32`, `A33` and `H-091026A09.png`. They are the last five entries of that folder in the server's list order; the four before them (`A34`–`A37`) kept their content. The upload itself is ruled out: a job ends as "Xong" only after a HEAD returns the local size, and `Etsy/Etsy-Chau/U-131026A13.png` from the same cycle has the same MD5 as its source on the NAS. 114 objects pushed at 16:07–16:09 were all intact minutes later. The writer is not this app: a public listing of the folder shows the five empty objects with `ChecksumAlgorithm` CRC32, the mark of an AWS SDK client and of 6,907 of the folder's 9,422 objects (the server's own uploader), while everything this app writes carries CRC64NVME, because `internal/s3up` sends no checksum and S3 adds its default. The same folder has earlier empty CRC32 objects from 2026-10-08 (3) and 2026-10-12 (5, again written in same-second groups), before this incident. Which server job writes them, and why with no content, is for the backend to find. The bucket also answers an anonymous list request, so anyone can enumerate its keys. The five files are no longer in the server's list, so nothing re-pushes them. Guard built the same day, see "Empty objects" above.
- Not known: whether the report call (`POST /api/v2/s3/missing-design-files/uploaded`) removes an entry from the queue API. If it does not, pushed rows stay in the list; unattended mode skips them while the app runs, and pushes them once more after a restart.
- LOW, older code — the S3 secret is passed to `/usr/bin/security` in argv and the binary is found through PATH (internal/s3up/store.go); the secret input lacks the password style (view.go).
- datefind does not walk `year/<designer>/<month>/…` (a non-date level under the year).
- Possible feature, waiting for what the user finds on the NAS: "Kiểm tra trực tiếp" for rows not found — re-list the folders of sibling files and match names that start with the row's name, with per-row confirmation.
- NAS: every folder named `Huyền HTN.Etsy` is unreadable from macOS (ENOENT even from `ls`); 974 unreadable folders in the last scan. Needs a rename on the NAS side (`convmv --nfc`).
- Manual app test (`CGO_ENABLED=0 go tool mygo dev`) has not been done for the File hỏng tab or the popup; "Đẩy tất cả" should wait for it.
- Search is by name only; no path, wildcard, size or date filters.


datefind done — internal/datefind/{names,find}.go + tests — tests pass
Destination folder picker done — dest.go dest_test.go upload.go view.go app.go — tests pass (42 subtests new, package ok)
report-done-rows — internal/brokenlist/report.go(+_test), broken.go, broken_rows.go, broken_report.go, broken_test.go, upload.go — tests pass
T-review-fixes done — dest.go dest_rows.go dest_view.go dest_test.go upload.go(selectedPaths) — tests pass
