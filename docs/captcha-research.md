# Nghiên cứu: cách handle captcha tốt hơn cho gateway NVPI

## TL;DR

Kiến trúc hiện tại (Chrome trực tiếp + pool) đang chạy đúng tư tưởng nhưng
**vô hình trung ăn theo đúng lối mòn mà hCaptcha thiết kế cho bot tự động hoá
phá**. Có 3 hướng cải thiện, xếp theo ROI:

| # | Hướng | Tác động | Công sức | Khuyến nghị |
|---|------|---------|----------|-------------|
| A | Dùng API key NVIDIA (`nvapi-`) thay captcha | **Loại bỏ hoàn toàn** captcha | Rất nhỏ | **Ưu tiên #1** nếu hữu dụng |
| B | Bổ sung solver API (CapSolver/NoneCap) làm fallback | Tăng reliability khi Chrome dép | Nhỏ | Nên làm |
| C | Tune pool/browser hiện tại (sticky, warm, budget) | Giảm cost, tăng TTFT | Trung bình | Làm song song |

Cái A là biggest win — nếu gateway chỉ phục vụ vài người dùng cá nhân, không
cần pool 3 Chrome chạy 24/7.

> **Quyết định 2026-08-22 (chủ project):** hướng A (nvapi-key) và B (solver
> trả phí) bị LOẠI vĩnh viễn — mục tiêu project là tránh nvapi-key (rate limit
> thấp) và giữ 100% miễn phí. Kiến trúc Chrome+hCaptcha pool là đích. Hướng C
> đã ship phần lớn: stickyMaxIdle 10m, mint batch nhiều widget/tab, Chrome
> elastic (`-chromes-max`), retry budget tách captcha/ladder, `stale_leases`
> lộ qua `/api/status`.

---

## Hiện trạng (đọc code đã xác minh)

`internal/captcha/` triển khai:

- `Browser` — 1 Chrome headless, 1 sticky tab `build.nvidia.com/minimaxai/minimax-m3/playground`.
  Steady-state: `hcaptcha.execute({async:true})` + poll `data-hcaptcha-response` (~300ms).
  Block image/font/css (`extract.go:30-47`), spoof `navigator.webdriver`, UA Chrome 131.
- `BrowserGroup` — N Chrome song song (multi-tab cùng Chrome **không mount widget**
  trên site này, `group.go:8`). Ladder recovery reload→relaunch.
- `Pool` — prewarm `Size` token, TTL `pool-ttl` (default 90s), FIFO, worker
  reserved-slot anti-over-mint, reaper dọn token stale, backoff exp + jitter.

Token hCaptcha + header `nv-captcha-token` là **vật duy nhất** gateway gửi xuống
predict endpoint (`executor.go:291-296`). Không cookie, không Authorization.

---

## A. Dùng API key NVIDIA thay captcha (ưu tiên #1)

Tìm thấy nguồn chính thức: NVIDIA cấp `nvapi-` API key cho playground/serverless
NIM tại `build.nvidia.com/settings/api-key`, dùng cho `integrate.api.nvidia.com`.
Key thuộc loại `AI_PLAYGROUNDS_KEY` (NGC docs liệt kê loại này).

**Tại sao đây là biggest win:** repo `nvidia-register` (github.com/zcz10086-dot)
chứng minh luồng: đăng ký tài khoản NVIDIA → tạo `AI_PLAYGROUNDS_KEY` → dùng cho
playground. **HCaptcha chỉ chắn bước đăng ký, không chắn predict endpoint** khi
có API key. Nghĩa là 1 lần đăng ký (captcha thủ công) → credentials dùng được
dài hạn, không cần Chrome pool.

**Trạng thái hiện tại của gateway:** nhận captcha **vì ghép với luồng anonymous
playground**. Nếu switch sang `Authorization: Bearer nvapi-...` thì predict
endpoint gọi trực tiếp, không cần `nv-captcha-token` header.

**Cần verify trước khi code** (tôi chưa test trực tiếp với repo này):
1. `predict` endpoint có chấp nhận `Authorization: Bearer nvapi-` thay cho
   `nv-captcha-token` không, hay endpoint khác (`integrate.api.nvidia.com`)?
2. Giới hạn rate/quota của `nvapi-` key với model playground (GLM-5.2) có thấp hơn
   anonymous không?

**Nếu verify OK:** thêm path `-nvidia-key nvapi-...` (hoặc env `NVIDIA_API_KEY`),
executor ưu tiên header `Authorization` thay `nv-captcha-token`. Xoá dependency
Chrome pool hoàn toàn cho use-case này.

---

## B. Solver API làm fallback

Khi `-auto` Chrome dép (bot detection, widget stale, IP bị flag), gateway hiện
trả 401/503. Lớp fallback rẻ:

| Đặc điểm | CapSolver | 2Captcha | NoneCap |
|---------|-----------|---------|---------|
| Mô hình | AI thuần | AI + human fallback | hCaptcha chuyên dụng |
| Giá /1k hCaptcha | ~$0.80 | ~$2-2.99 | $0.20-0.50 |
| Latency hCaptcha | 3-8s | 15-40s | ?block 90s |
| hCaptcha success | 93% std, 85% ent | 98% | real P1_ token |
| API | createTask/getTaskResult | in.php/res.php + JSON | /v1/solves?wait=N |

Nguồn: docs capsolver.com, 2captcha.com/pricing, benchmark brightdata/scrapewise 2026.

**Cái quan trọng nhất từ research:** token hCaptcha **single-use, ~120s TTL,
bind IP/session nếu enterprise** (nonecap rqdata). NVPI gửi only `nv-captcha-token`
vào predict, không cookie → khả năng cao playground dùng hCaptcha **thường
(regular)** không enterprise rqdata (không cookie/session binding chặt). Nghĩa
là solver API trả token có thể submit được nếu **đồng bộ IP exit** giữa solve và
submit — đó là thực tế quan trọng nhất mà code hiện tại bỏ qua.

**Thực tế IP:** gateway chạy Chrome và gọi predict từ cùng host → IP exit đồng bộ
theo tự nhiên. Solver API remote thì IP solver ≠ IP gateway → nếu playground
check IP-submit==IP-solve, solver token bị reject. Cần test thực tế.

**Tích hợp đề xuất (nhỏ):** thêm interface `CaptchaSource` với 2 impl:
`ChromePool` (hiện tại) + `SolverAPI`. Executor thử Chrome trước, fail/hang >
threshold → solver. Làm flag `-solver-key` + `-solver-url`. Không thay kiến trúc
pool, chỉ thêm một nguồn token nữa.

---

## C. Tune pool/browser hiện tại

Một số tinh chỉnh có thể có giá trị, dựa trên đọc code nhưng **chưa benchmark**:

1. ~~**`stickyMaxIdle` 60s có thể quá ngắn cho workload chat**~~ — ĐÃ LÀM:
   nâng 10m, đối xứng với `-chrome-idle-recycle 10m`.
2. **TTL pool 90s vs thực tế:** token ~120s TTL. TNTL 90s an toàn nhưng waste
   ~25% lifetime. Nếu pool turnover cao, tăng TTL ~110s giảm fill rate.
3. **Reaper interval `ttl/4`** = 22.5s — khá dày, token có thể stale tới 22s
   trước khi reaped. Đặt `min(interval, 10s)` nếu workload latency-sensitive.
4. ~~**`maxAttempts=3` trong doPredict**~~ — ĐÃ LÀM: tách 2 budget
   (`maxCaptchaRetries=2`, ladder ≤5, worst case 8 send), `stale_leases`
   đếm riêng và lộ qua `/api/status`.
5. **Chrome warm trên 1 model cố định** (`playgroundURL` = minimax-m3) — nếu registry
   có nhiều model, mỗi request tới model khác force re-navigate. Cần warm theo
   model hoặc cache sticky tab per model (phức toán hơn).

Những cái C này chỉ đáng làm khi A/B chưa đủ. Ponytail nguyên tắc: fix root
(A) trước, rồi mới bàn tune.

---

## Khuyến nghị

1. **Test A trước** — chạy thử `PredictEndpoint` với `Authorization: Bearer nvapi-`
   thay `nv-captcha-token` trên 1 request thủ công. 30 phút verify, nếu OK là
   loại bỏ được cả `internal/captcha/` (1900 dòng) cho use-case cá nhân.
2. Nếu A không khả thi (rate limit/endpoint khác) → làm **B-minimal**: thêm
   1 solver fallback sau Chrome, flag opt-in, verify IP-match trước.
3. C chỉ khi đã xong A/B và vẫn còn headroom cần tối ưu.

Bước tiếp theo cụ thể: tôi chạy thử một request predict với `nvapi-` key (cần
user cấp key hoặc tự đăng ký) để verify điểm A. Muốn tôi viết probe script
để verify không?
