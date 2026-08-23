# NVPI — Gateway NVIDIA Playground miễn phí cho Claude Code

[![License: MIT](https://img.shields.io/badge/License-MIT-blue.svg)](./LICENSE)
[![Go](https://img.shields.io/badge/Go-1.26-00ADD8?logo=go&logoColor=white)](https://go.dev)

Gateway local biến **NVIDIA Playground** thành API chuẩn OpenAI/Claude/Responses — **không cần API key**, không tốn phí, thiết kế xoay quanh **Claude Code** làm client chính. hCaptcha one-shot được mint tự động bằng headless Chrome pool.

> Model mặc định: `minimaxai/minimax-m3` (1M context). `z-ai/glm-5.2` đã retire phía NVIDIA.
>
> **Ràng buộc thiết kế** (không thương lượng): không dùng `nvapi-key` (rate limit thấp), không dùng dịch vụ captcha-solver trả phí — mọi thứ 100% miễn phí. Chi tiết nghiên cứu: [docs/captcha-research.md](docs/captcha-research.md).

## Kiến trúc

```
Claude Code / OpenAI SDK ──► NVPI gateway (:8080, CLIProxyAPI embedded)
                              │  translators: OpenAI ↔ Claude ↔ Responses
                              │
                    ┌─────────▼──────────┐
                    │  Captcha token pool │  prewarm FIFO, TTL thích ứng 60–115s,
                    │  batch mint ×N      │  retry budget riêng, reaper + backoff
                    └─────────┬──────────┘
                              │ hcaptcha.execute trên sticky tab
                   ┌──────────▼───────────┐
                   │ Elastic Chrome group  │  1 → chromes-max khi burst,
                   │ (chromedp)            │  thu hồi idle > chrome-idle-recycle
                   └──────────┬───────────┘
                              ▼
              POST api.ngc.nvidia.com/v2/predict/... (anonymous)
```

Mỗi request chỉ tiêu **1 token one-shot** (`nv-captcha-token`) — không cookie, không Authorization.

## Quick start

```bash
# Cần Chrome đã cài trên máy (chromedp tự tìm; CHROME_PATH để chỉ đường dẫn)
go run ./cmd/serve -auto -default-model=minimaxai/minimax-m3
```

Kết nối **Claude Code**:

```bash
export ANTHROPIC_BASE_URL=http://127.0.0.1:8080
export ANTHROPIC_AUTH_TOKEN=dummy   # gateway không kiểm tra key inbound
claude
```

`-default-model` rewrite các model `claude-*` mà NVIDIA không có về model đăng ký — không có flag này thì request model lạ bị 400 nghiêm ngặt.

Hoặc gọi thẳng:

```bash
curl http://127.0.0.1:8080/v1/chat/completions \
  -H 'Content-Type: application/json' \
  -d '{"model":"minimaxai/minimax-m3","messages":[{"role":"user","content":"Hi"}],"stream":true}'
```

## Endpoint

| Đường | Nội dung |
|---|---|
| `POST /v1/chat/completions`, `/v1/responses`, `/v1/messages` | 3 format API, translator tích hợp sẵn |
| `GET /` | **Web dashboard** (nhúng trong binary): pool/chromes/limits tự poll 5s, bảng model, ô thử chat |
| `GET /api/status` | Số liệu thật JSON: fills/takes/expired/stale_leases/ttl, chromes live/max, limits |
| `GET /api/models` | Registry hiện hành kèm contextLength + capability |
| `GET /healthz`, `GET /hello`, `GET /api/hello` | Liveness + số liệu pool/chrome |

## Cấu hình

| Flag | Mặc định | Ý nghĩa |
|---|---|---|
| `-addr` | `127.0.0.1:8080` | Bind loopback vì gateway **không auth**; Docker/compose truyền `:8080` tường minh |
| `-auto` | off | Bật Chrome pool prewarm (không bật thì mỗi request phải tự gửi `nv-captcha-token`) |
| `-pool-size` | `6` | Số token ready giữ trong buffer |
| `-pool-workers` | `3` | Worker mint song song (mỗi worker mượn 1 Chrome) |
| `-pool-batch` | `6` | Token mint mỗi lần mượn Chrome (nhiều widget ẩn trên cùng sticky tab) |
| `-chromes-max` | `2` | Trần Chrome elastic; `1` = cố định. Mặc định thấp để 1 batch phủ burst — ít Chrome, ít RAM |
| `-chrome-idle-recycle` | `10m` | Đóng Chrome idle quá hạn (giữ floor 1); đối xứng sticky-tab staleness |
| `-max-inflight` | `8` | Trần stream đồng thời — đủ cho burst parallel tool-call của Claude Code |
| `-inflight-wait` | `500ms` | Chờ slot inflight trước khi 503 |
| `-coalesce-ms` | `16` | Gộp SSE delta liên tiếp (token đầu luôn flush ngay — TTFT) |
| `-warm-timeout` | `3m` | Chờ ≥1 token trước khi serve |
| `-pool-ttl` | `90s` | Tuổi tối đa token buffer (tự trôi 60–115s theo tỉ lệ stale) |
| `-captcha-wait` | `30s` | Chờ token khi pool cạn rồi mới 503 |
| `-captcha-playground` | rỗng = auto | Rỗng: bench cửa sổ trượt 5 trang sống mỗi lần start (`-captcha-select-budget` 4m), champion lưu `~/.nvpi/playground-state.json`; NVIDIA retire model → tự đổi trang mint, không hardcode. Gắn URL để ghim cứng. Bỏ qua khi `-captcha-harness` bật (mặc định) — mint không phụ thuộc trang model sống |
| `-captcha-harness` | `true` | Mint trên **trang harness tối giản** thay vì trang Next.js đầy đủ: cùng origin build.nvidia.com, sitekey scrape bằng HTTP thuần lúc start — RAM/chrome ~50MB thay vì ~150–350MB, warm ~2s thay vì 6–10s, mint batch 6 token ~2s. Scrape fail → tự fallback về đường cũ (kèm playground selection) |
| `-default-model` | rỗng | Rewrite model lạ (VD `claude-*`) về model đăng ký |
| `-refresh-registry` | `true` | Refresh catalog NVIDIA lúc start (merge, không thay thế — probe fail tạm thời không làm mất model) |
| `-chrome-proxy` | `CHROME_PROXY` | Proxy cho cả Chrome lẫn upstream (vd `socks5://host:port`) |

## Docker

```bash
docker compose up --build -d     # -auto -addr :8080, shm 2GB cho Chrome
```

## Công cụ phụ

| Lệnh | Dùng để |
|---|---|
| `cmd/captchabatch` | Bench mint batch trên group elastic |
| `cmd/hangbench` | Bench độ trễ/khung treo pool |
| `cmd/streambench` | Bench SSE coalescing |
| `cmd/getsitkey` | In sitekey hCaptcha bằng cách nghe network traffic |
| `cmd/probehard` | Probe registry hardcode theo tín hiệu `modelCapability` — liệt kê model retire để prune |
| `cmd/cacheprobe`, `cmd/captchaopt` | So chiến lược extract/probe |

## License

[MIT](./LICENSE) © 2026 6Kmfi6HP
