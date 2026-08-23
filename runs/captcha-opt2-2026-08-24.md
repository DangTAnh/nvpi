# captcha-opt round 2 — 2026-08-24 (HEAD 35c6f70 → 3dd24af)

Research question: còn tối ưu được RAM chrome / tốc độ captcha không, hay đã chạm trần?
Method: 2 research agents (repo audit + external web research), rồi benchmark trước/sau từng thay đổi
trên cùng workload, cùng khung giờ; tệ hơn thì revert. Raw: `captcha-opt2-raw-2026-08-24.txt` (log phiên),
số liệu trong bảng dưới + `bench-baseline-2026-08-24.txt`.

## Baseline (HEAD 35c6f70, đêm 23–24/08)

| Metric | Value |
|---|---|
| bench-pages sweep (58 models, -rounds 3) | 28 models alive, median-of-medians exec **1.206s**, worst nav ~17s |
| pocharness `-n 1` ×3 | 1.922 / 2.036 / 2.045s → median **2.04s** |
| pocharness `-n 6` | **3.617s**, predict HTTP 200 |
| RAM peak toàn cây chrome khi mint n=6 | **623MB** (6 processes = 1 Chrome) |
| FetchSitekeyHTTP | **65–68s mỗi lần serve start** |

## Kết quả từng thay đổi

### 1. `--js-flags=--max-old-space-size=128` (heap cap V8, harness-only) → REVERT

pocharness `-n 1`: 3.17 / 3.756 / 3.589s (+60–80% so baseline). Đối chứng ngay sau đó không cap:
2.114 / 1.886s → regression là thật, không phải nhiễu mạng. Đã drop khỏi tree.
Bài học: heap cap làm GC pacing xấu đi trên đúng đường hcaptcha cần tốc độ; không thử lại.

### 2. Preconnect hcaptcha origins trong harness HTML → KEEP (1199033)

`<link rel=preconnect>` cho js.hcaptcha.com + newassets.hcaptcha.com trước thẻ api.js.
pocharness `-n 1` ×3: 1.935 / 1.854 / 1.857s → median **1.86s (−9%)**, cả 3 run đều dưới min baseline.
RAM không đổi (n6 peak 620MB).

### 3. Disk-cache sitekey (<24h TTL) → KEEP (3dd24af)

Bỏ crawl ~65s mỗi lần restart. Verify end-to-end: cache ghi đúng key, restart log
"cached sitekey", tới pool-ready **~3s thay vì ~68s**. Unit test roundtrip/expiry/corrupt PASS.
Đánh đổi có chủ đích: NVIDIA rotate key trong cửa sổ TTL → mints hỏng đến hết TTL hoặc xoá file;
hiếm, và rotate giữa chừng vốn đã yêu cầu restart để chữa.

## Đã ở sàn (không còn dư địa đáng khai thác)

- Sticky execute ~300–340ms: do backend hCaptcha, Go-side không cắt được.
- Token cache đã bám TTL thật [60s,115s]; multi-tab bị site chặn nên batch-widget là tối đa mỗi visit.
- Asset blocking (images/CSS/font/media) đã xong từ round trước; Fetch-interception đo chậm hơn.
- Không còn sleep/networkidle cứng; các poll còn lại 50–100ms CDP evaluate (rẻ).
- Cold Chrome init ~2s: bound bởi Chrome + hcaptcha api.js; chỉ còn mẩu (preconnect đã lấy phần lớn).
- `--single-process`/`--no-zygote`: no-op hoặc crash trên Windows (web research, có nguồn); bỏ.
- MB/widget đo được: **~28MB/widget biên** ((620−481)/5). Hạ `-pool-batch` 6→4 tiết kiệm ~56MB/chrome
  duy trì nhưng refill lạnh thêm ~3–4s — trade trực tiếp speed↔RAM, để flag cho người dùng tự chọn.

## Chưa làm (đánh giá đủ, không đủ lý do làm ngay)

- Split-first-fill mint (mint 1 ngay rồi top-up): first token sớm hơn ~3s lúc lạnh nhưng full-pool chậm hơn;
  `-warm-timeout` đang gate serving nên visibility thấp.
- Event-driven token read thay poll 50ms: ≤50ms/mint, effort M.
- Re-bench default workers w=3 vs chromes-max=2: không ảnh hưởng RAM (Chrome mới tốn RAM, worker không).

## Kết luận

Còn tối ưu được và đã lấy: **preconnect −9% cold mint**, **restart nhanh ~65s**. Sweep xác nhận sau
cả hai commit (`bench-after-2026-08-24.txt`): 28/58 alive, median-of-medians **1.199s** vs 1.206s
baseline — nhiễu thuần, fallback path không đổi. Phần còn lại đã chạm trần kỹ thuật (hCaptcha execute
roundtrip, TTL token, site behavior). Các lever ngoài còn lại (shared browser + incognito tab,
virtual time budget) bị loại vì multi-tab hỏng trên site này / rủi ro fingerprint — chi tiết trong raw log.
