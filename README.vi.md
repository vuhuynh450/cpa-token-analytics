# CPA Token Analytics

[English](README.md) | Tiếng Việt

**Ưu tiên local, chạy nhẹ.** Một **plugin CLIProxyAPI** local-first, nhẹ cho phân tích token usage.

- **Ưu tiên local**: dữ liệu usage được lưu trong SQLite trên máy của bạn, không upload lên nền tảng analytics bên thứ ba; plugin chỉ chạy cùng CLIProxyAPI của bạn.
- **Thu thập thời gian thực**: thông qua plugin callback `usage.handle`, mỗi request được ghi vào database ngay khi hoàn tất — không polling, không cửa sổ queue, không mất dữ liệu.
- **Không cần process phụ**: dynamic library gốc (`.dylib` / `.so`) chạy ngay trong process CPA — không cần Python, Node.js, Docker hay service riêng.
- Giao diện shadcn/ui thật: xu hướng token, các chế độ xem theo model / key ẩn danh / chi tiết request, ước tính chi phí, hiệu năng model và request inspector kèm bộ lọc.

**Trạng thái: v0.4.0 / CLIProxyAPI 7.x+ / macOS, Linux và Windows. Mã nguồn mở MIT.**

## Tính năng

- **Cập nhật trực tiếp**: dashboard poll một endpoint revision nhẹ (`/api/changes`) và chỉ tải lại dữ liệu thật khi có usage event mới — gần thời gian thực mà không cần WebSocket hay query nặng. Chọn chu kỳ (3/5/10 giây) hoặc tạm dừng; tự tạm dừng khi tab bị ẩn.
- **Phân tích chi phí**: ước tính USD tương đương API từ pricing catalog đã đồng bộ + token counters từng event, kèm price snapshot lịch sử, override theo model, trạng thái coverage partial/unpriced, và quy đổi USD→VND. Đây là **ước tính**, không phải hóa đơn thật — traffic OAuth/subscription có thể được provider tính phí khác.
- **Pricing model tự động**: catalog đồng bộ từ Models.dev, fallback LiteLLM, match theo exact/alias/normalized; hỗ trợ override giá thủ công.
- **Tỉ giá USD/VND tự động**: lấy từ ExchangeRate-API (fallback Fawaz currency-api), cache trong SQLite kèm nguồn/timestamp; có chế độ manual cho tỉ giá cố định.
- **Hiệu năng model**: số request, latency, TTFT và percentile theo model.
- **So sánh usage**: số liệu kỳ hiện tại vs kỳ trước (hôm nay/7d/30d/tùy chọn, elapsed hoặc full-period).
- **Biểu đồ nâng cao**: heatmap usage, top models, phân bố token, xu hướng hiệu năng.
- **Request Inspector**: xem từng request với phân trang server-side và bộ lọc — status, latency, ngưỡng token/chi phí, search — kèm dialog chi tiết breakdown token, timing và chi phí ước tính.

## Yêu cầu

- CLIProxyAPI có hỗ trợ native plugin (đã test với CLIProxyAPI 7.x/8.x).
- Một trong các OS/architecture trong mục [Nền tảng hỗ trợ](#nền-tảng-hỗ-trợ).
- Không cần service phụ, database server hay API key: mọi thứ chạy trong process CPA và lưu local.

## Cài đặt

### Cài đặt qua custom Plugin Store (khuyến nghị)

1. Mở **CLIProxyAPI Management Panel** → **Plugin settings** → **Third-Party Plugin Sources**.
2. Thêm registry URL:

   ```
   https://raw.githubusercontent.com/vuhuynh450/cpa-token-analytics/main/plugin-store/registry.json
   ```

3. Lưu danh sách source.
4. Mở **Plugin Store** → tìm **CPA Token Analytics** → **Install**.
5. Bật và cấu hình plugin (xem [Cấu hình](#cấu-hình)).
6. Mở dashboard tại `/v0/resource/plugins/cpa-token-analytics/report`.

> Registry URL chỉ hoạt động khi repository đã public **và** đã có release kèm plugin asset — store cài binary plugin từ file đính kèm của GitHub release, không phải từ chính registry. Trường `version` trong `registry.json` chỉ là hiển thị dự phòng; GitHub release mới nhất mới là nguồn thật.

Tương đương khi cấu hình trực tiếp qua `cliproxyapi.conf`:

```yaml
plugins:
  enabled: true
  store-sources:
    - "https://raw.githubusercontent.com/vuhuynh450/cpa-token-analytics/main/plugin-store/registry.json"
  configs:
    cpa-token-analytics:
      enabled: true
```

> CPA truy vấn GitHub API để lấy thông tin plugin với giới hạn tốc độ ẩn danh; nếu gặp rate limit, hãy đặt biến môi trường `GITHUB_TOKEN` cho process CPA và thêm rule `type: github-token`, `token-env: GITHUB_TOKEN` trong `plugins.store-auth`.

### Cài đặt thủ công (offline)

Tải `cpa-token-analytics_{version}_{goos}_{goarch}.zip` tương ứng nền tảng của bạn từ [Releases](https://github.com/vuhuynh450/cpa-token-analytics/releases), giải nén và đặt vào thư mục plugins:

```bash
unzip cpa-token-analytics_0.4.0_darwin_arm64.zip
mkdir -p <plugins-dir>/darwin/arm64
cp cpa-token-analytics-v0.4.0.dylib <plugins-dir>/darwin/arm64/
# Linux dùng .so, tên thư mục tương ứng là linux/amd64 hoặc linux/arm64
```

Hoặc build từ source:

```sh
cd plugin
go build -buildmode=c-shared -o cpa-token-analytics.dylib .   # macOS; artifact trên Linux là .so
```

## Di chuyển từ usage-report

Plugin trước đây phân phối với tên `usage-report`; nay là plugin độc lập với id `cpa-token-analytics` (official store đã có một plugin `usage-report` khác). Dữ liệu hiện có vẫn an toàn:

1. **Database.** File SQLite mặc định mới là `~/.cli-proxy-api/cpa-token-analytics.sqlite`. Lần chạy đầu, nếu file mới chưa tồn tại nhưng `~/.cli-proxy-api/usage-report.sqlite` có, plugin sẽ **copy** database cũ sang vị trí mới — file cũ được giữ nguyên, không xóa không đụng. Nếu muốn dùng trực tiếp file cũ, đặt `plugins.configs.cpa-token-analytics.db_path` trỏ vào nó.
2. **Config key.** Đổi `plugins.configs.usage-report` thành `plugins.configs.cpa-token-analytics` trong `cliproxyapi.conf` (hoặc tương đương trong management panel).
3. **Tắt plugin cũ.** Gỡ hoặc disable `usage-report` để tránh hai plugin cùng thu thập một luồng traffic.
4. **Mọi thứ đi theo cùng nhau.** Pricing catalog, tỉ giá FX, ước tính chi phí và lịch sử request đều nằm trong cùng file SQLite nên sẽ chuyển theo nó.

Các route chuyển từ `/v0/{resource,management}/plugins/usage-report/...` sang `/v0/{resource,management}/plugins/cpa-token-analytics/...` — hãy cập nhật bookmark hoặc script gọi API trực tiếp.

## Sử dụng

- Trang: `/v0/resource/plugins/cpa-token-analytics/report`
- Data API (yêu cầu xác thực): `/v0/management/plugins/cpa-token-analytics/api/usage?start=YYYY-MM-DD&end=YYYY-MM-DD&bucket=day|hour|month`, với header `Authorization: Bearer <management key>`
- Mọi request đi qua instance CPA này (Codex CLI, OpenCode, các client khác) đều được tự động ghi lại.

Khả năng của giao diện:

- Các khu vực overview, chi phí, hiệu năng, so sánh, request inspector và pricing; live updates có thể tạm dừng hoặc chỉnh nhịp (3/5/10 s).
- Hôm nay, 7/30 ngày gần nhất và khoảng ngày tùy chỉnh; bộ lọc model áp dụng đồng thời cho KPI, trend và chi tiết.
- Trend chồng lấp của cached input / net input / output, độ chi tiết giờ / ngày / tháng.
- Các góc nhìn theo model, key ẩn danh và từng request — kèm tìm kiếm, sắp xếp, phân trang và bộ lọc request nâng cao (trạng thái, độ trễ, kích thước token, chi phí).
- Ước tính chi phí tự động từ pricing catalog đồng bộ, cùng chỉ số độ trễ, TTFT và tốc độ output từng request; usage của model chưa biết không bị ngụy trang thành hóa đơn thực tế.
- Mọi request đều được tính vào thống kê tỉ lệ thành công, kể cả các lỗi zero-token; input bao gồm cached token, output bao gồm reasoning token, không cộng trùng.
- Lỗi, dữ liệu trống và bất thường khi thu thập được hiển thị rõ ràng — dữ liệu trắng hoặc cũ không bị coi là thành công thời gian thực.

## Đa ngôn ngữ (i18n)

Giao diện song ngữ — English và Tiếng Việt:

- Công tắc ngôn ngữ (Select) nằm trong header, cạnh nút chuyển theme dark/light.
- Lựa chọn được lưu trong trình duyệt qua localStorage key `cpa-usage-locale`.
- Mặc định: đọc localStorage trước; nếu chưa có, ngôn ngữ trình duyệt quyết định — `vi*` dùng Tiếng Việt, các trường hợp còn lại dùng English.
- Số và ngày được format theo locale (`en-US` / `vi-VN`); cả hai ngôn ngữ dùng chung một dữ liệu nền.

## Cấu hình

| Trường | Mô tả |
|---|---|
| `plugins.configs.cpa-token-analytics.enabled` | Có bật plugin hay không |
| `plugins.configs.cpa-token-analytics.db_path` | Đường dẫn SQLite, mặc định `~/.cli-proxy-api/cpa-token-analytics.sqlite` |
| `plugins.store-sources` | Các địa chỉ registry của plugin store (dùng khi cài đặt/cập nhật) |

## Nền tảng hỗ trợ

Mỗi release phát hành một zip cho từng nền tảng — `cpa-token-analytics_{version}_{goos}_{goarch}.zip` — kèm `checksums.txt` để kiểm tra toàn vẹn, theo convention của CLIProxyAPI plugin store:

- `linux/amd64`, `linux/arm64`
- `darwin/amd64`, `darwin/arm64`
- `windows/amd64`

## Vị trí SQLite

Mặc định, sự kiện usage được ghi vào `~/.cli-proxy-api/cpa-token-analytics.sqlite` (chế độ WAL). Có thể ghi đè bằng `plugins.configs.cpa-token-analytics.db_path`. Gỡ cài đặt plugin **không** xóa các file `~/.cli-proxy-api/*.sqlite`.

## Quyền riêng tư

Thiết kế local-first: sự kiện usage, key, dữ liệu chi phí và lịch sử request không bao giờ rời máy này. API key chỉ hiển thị dưới dạng nhóm ẩn danh ổn định (`key-xxxxxxxxxx`, dựa trên sha256) hoặc `Unknown Key`. Traffic ra ngoài duy nhất là việc fetch pricing catalog và tỉ giá FX (tùy chọn) phục vụ ước tính chi phí — không usage data nào được gửi đi.

## Hoạt động offline

Dashboard và mọi phân tích chạy hoàn toàn offline. Chỉ tính năng đồng bộ pricing catalog và tỉ giá FX cần kết nối dịch vụ bên ngoài; thiếu chúng mọi thứ vẫn hoạt động, các trường chi phí chỉ hiển thị unavailable.

## Cập nhật

Bản cài qua Plugin Store theo dõi GitHub release mới nhất: refresh danh sách store và nhấn **Update** khi có release mới. Với cài thủ công, tải zip mới và thay file library.

## Xử lý sự cố

- **Plugin không load**: kiểm tra binary có khớp `goos`/`goarch` của bạn không (và phiên bản glibc trên Linux — plugin là native shared library). Library sai kiến trúc hoặc không tương thích sẽ không load được nhưng cũng không đụng vào dữ liệu của bạn.
- **Plugin không xuất hiện trong store**: registry URL phải truy cập được và repo phải public; xác nhận source đã được lưu trong Third-Party Plugin Sources.
- **Cài đặt bị rate-limit**: đặt `GITHUB_TOKEN` và rule `plugins.store-auth` như mô tả ở trên.

## Dữ liệu & Bảo mật

- Sự kiện usage được lưu trong SQLite local do `db_path` chỉ định (bảng `usage_events`); API của trang chuyển key thành các nhóm ẩn danh ổn định (`key-xxxxxxxxxx` hoặc `Unknown Key`) trước khi trả dữ liệu.
- Sự kiện gốc chỉ lưu trên máy local và có thể chứa key của client — không upload database lên nơi công khai.
- Trang tài nguyên của plugin không có xác thực riêng, chỉ truy cập được trong management domain của CPA; không expose management port của CPA ra internet công cộng.

## Phát triển

```sh
cd plugin
go test ./...       # unit test
go build -buildmode=c-shared -o cpa-token-analytics.dylib .  # macOS; trên Linux là .so
```

Tài nguyên trang nằm trong `plugin/web/`, là build output của `frontend/` trong repo này:

```sh
cd frontend && npm ci && npm run build
cp dist/index.html ../plugin/web/index.html
cp dist/assets/* ../plugin/web/assets/
```

> Sau khi thay frontend phải cập nhật đồng bộ tên file embed trong `plugin/main.go` và route tài nguyên trong `management.register` (build artifact có tên file kèm hash).

### Phát hành phiên bản mới

Số phiên bản nằm trong `VERSION`, khớp với git tag (`v{version}`). Push tag sẽ kích hoạt Release pipeline: build plugin cho năm nền tảng (darwin/arm64, darwin/amd64, linux/amd64, linux/arm64, windows/amd64), inject số phiên bản, tạo `checksums.txt` và publish Release.

```sh
# 1. Cập nhật VERSION và CHANGELOG.md, commit và merge vào main
# 2. Tạo tag và push; pipeline tự động publish
git tag v0.4.0
git push origin v0.4.0
# 3. Cập nhật trường version trong registry của plugin store
```

## Giấy phép

[MIT](LICENSE). Component giao diện dựa trên [shadcn/ui](https://github.com/shadcn-ui/ui) (MIT); xem [THIRD_PARTY_NOTICES.md](THIRD_PARTY_NOTICES.md).
