# 字节火山引擎 TTS v3 → OpenAI 兼容接口

将火山引擎 TTS v3 API 封装为 OpenAI 兼容的 `/v1/audio/speech` 端点,自带引导式安装、WebUI 后台、SQLite 持久化、Prometheus 观测。单二进制,无外部服务依赖。

## 特性

- 完全兼容 OpenAI `/v1/audio/speech` API
- **引导式安装**: 首次启动自动进入 `/setup` 向导,无需手写 env
- **WebUI 后台** `/admin`: 音色管理 / 全局设置 / CORS 配置 / 鉴权
- **音色库**: 多 voice 动态路由,未知 / 禁用 voice 返回明确错误
- **多格式输出**: mp3 / ogg_opus / pcm / wav (内部转 pcm + 本地拼头) / aac / flac
- **鉴权 + 限流**: API Key 校验, IP 速率限制, 全局并发限制
- **观测**: Prometheus 文本格式 `/metrics` + 服务状态 `/health`, 零外部依赖
- **跨平台**: Windows / Linux / macOS / Docker

## 快速开始

### 1. 编译

```bash
go build -o tts-api .
```

### 2. 启动

```bash
# 默认: 端口 8080, 监听 localhost
./tts-api
```

启动后浏览器打开 `http://localhost:8080/setup`,完成 4 步引导:

1. 凭证: 火山 API Key + OpenAI 端鉴权 key
2. 默认路由: 默认资源 ID (复刻 2.0 用 `seed-icl-2.0`) + 默认音色名
3. 音色列表: 每个 voice 的对外名 + 火山 speaker ID
4. 确认提交

完成后自动跳转到 `/admin`,从这里登录管理。

### 3. 调用

```bash
curl -X POST http://localhost:8080/v1/audio/speech \
  -H "Authorization: Bearer <装时设的 OpenAI key>" \
  -H "Content-Type: application/json" \
  -d '{"model":"tts-1","input":"你好,世界","voice":"chun"}' \
  -o output.mp3
```

## 环境变量

**只需 4 个启动引导变量**(安装前 / DB 还不存在时必需):

| 变量 | 必设 | 用途 |
|---|---|---|
| `TTS_ADMIN_KEY` | ✓ (生产) | `/api/setup` 安装 token,首次安装时校验 |
| `TTS_DB_PATH` | ✗ | DB 路径,默认 `./tts.db` |
| `PORT` | ✗ | 监听端口,默认 `8080` |
| `OPENAI_TTS_API_KEY` | ✗ (生产) | OpenAI 端鉴权 key,可不设(内网) |

**所有业务配置**(`api_key` / `default_resource_id` / `default_speaker` / `default_format` / `sample_rate` / `model` / `model_type` / `explicit_language` / `enable_subtitle` / `timeout` / `cors_origins` / `cors_allow_all` / `auth_key` / `trusted_proxy_hops`) — 全部在 `/admin` 设置,持久化到 SQLite。

env 仍可作为 fallback 读(老用户兼容),但**新用户应通过 WebUI 配**。

## WebUI 使用

### `/setup` 首次安装

四步表单:
1. **凭证**: 火山 API Key (必填) + OpenAI 鉴权 Key (可选)
2. **默认路由**: 默认资源 ID (`seed-icl-2.0`) + 默认音色名 + 默认格式 + 采样率
3. **音色列表**: 每个 voice 一行,填对外名 + 火山 speaker ID
4. **确认**: 提交写入 DB,自动跳 `/admin`

### `/admin` 日常管理

| Tab | 用途 |
|---|---|
| 仪表盘 | 进程状态 / 内存 / 启动时长 / 配置检查 |
| 音色管理 | CRUD 音色: 名称 / speaker / 资源 ID / 模型 / 语言 / 启用 / 描述 |
| 设置 | 全局设置: API Key / 默认资源 ID / 默认音色 / 格式 / 采样率 / 子模型 / 鉴权 key |
| CORS | 白名单 (逗号分隔) 或 `*` 模式 |
| 退出 | 清除 session |

修改设置后**自动 reload**,不需重启服务。

### 客户端可见错误

| 场景 | HTTP | Body code | message |
|---|---|---|---|
| 缺少 Authorization | 401 | `invalid_api_key` | Invalid API key provided. |
| 音色不存在 | 400 | `unknown_voice` | `unknown voice: 'alloy'` |
| 音色被 admin 禁用 | 403 | `voice_disabled` | `voice 'chun' is disabled` |
| 服务未就绪 (DB 配置损坏) | 503 | `service_unavailable` | TTS service configuration error... |

## API 端点

### `POST /v1/audio/speech`

OpenAI 兼容,鉴权 `Authorization: Bearer <OPENAI_TTS_API_KEY>`(若已设)。

请求体:

```json
{
  "model": "tts-1",          // 兼容字段,实际不影响
  "input": "你好,世界",     // 必填,要合成的文本
  "voice": "chun",           // 选填: 装时设的 voice 名称,留空走 default_speaker
  "response_format": "mp3",  // 选填: mp3 / opus / wav / pcm / aac / flac
  "speed": 1.0               // 选填: 0.25 ~ 4.0,实际 0.5 ~ 2.0 生效
}
```

格式映射:

| OpenAI `response_format` | 上游实际 | Content-Type |
|---|---|---|
| `mp3` (默认) | mp3 | `audio/mpeg` |
| `opus` | ogg_opus | `audio/ogg` |
| `wav` | pcm → 本地拼 wav header | `audio/wav` |
| `pcm` | pcm | `audio/pcm` |
| `aac` / `flac` | mp3 (降级) | `audio/mpeg` |

### `GET /health`

无鉴权,返回:

```json
{
  "status": "ok",                            // ok | not_installed | configuration_error
  "service": "ByteDance TTS to OpenAI API Adapter",
  "version": "v0.2.0",
  "commit": "621f9f8",
  "uptime": "3600 seconds",
  "config_status": {
    "all_required_vars_set": true,
    "config_error": false,
    "error": "..."                           // 仅在 config_error=true 时出现
  },
  "installed": true,
  "mode": "normal"
}
```

- 正常: HTTP 200, `status: "ok"`
- 未安装: HTTP 200, `status: "not_installed"`
- 配置损坏: HTTP 503, `status: "configuration_error"`, `error` 字段有原因

### `GET /metrics`

Prometheus 文本格式,无鉴权。主要指标见 [观测 / Metrics](#观测--metrics)。

### `GET /admin`, `GET /setup`, `GET /dashboard`

- `/admin`: 后台 (Vue SPA),需鉴权
- `/setup`: 引导式安装页 (Vue SPA),无鉴权,装完自动跳转
- `/dashboard`: 服务状态预览页 (无鉴权)

## 老用户迁移 (从 v0.1.0 → v0.2.0)

v0.1.0 用 env 配 11 个 `BYTEDANCE_TTS_*` 变量。v0.2.0 起改 WebUI:

| 旧 env | v0.2.0 配置入口 |
|---|---|
| `BYTEDANCE_TTS_API_KEY` | `/admin` 设置 → API Key |
| `BYTEDANCE_TTS_RESOURCE_ID` | `/admin` 设置 → 默认资源 ID |
| `BYTEDANCE_TTS_SPEAKER` | `/admin` 设置 → 默认音色 (填 voice **名字**,不是 speaker ID) |
| `BYTEDANCE_TTS_FORMAT` | `/admin` 设置 → 默认格式 |
| `BYTEDANCE_TTS_SAMPLE_RATE` | `/admin` 设置 → 采样率 |
| `BYTEDANCE_TTS_BIT_RATE` | `/admin` 设置 → MP3 比特率 |
| `BYTEDANCE_TTS_MODEL` | `/admin` 设置 → 子模型 |
| `BYTEDANCE_TTS_MODEL_TYPE` | `/admin` 设置 → 模型类型 |
| `BYTEDANCE_TTS_EXPLICIT_LANGUAGE` | `/admin` 设置 → 显式语言 |
| `BYTEDANCE_TTS_ENABLE_SUBTITLE` | `/admin` 设置 → 启用字级时间戳 |
| `BYTEDANCE_TTS_TIMEOUT` | (保留 env 暂未搬 DB) |
| `ALLOWED_ORIGINS` | `/admin` → CORS → 跨域白名单 |
| `OPENAI_TTS_API_KEY` | `/admin` 设置 → OpenAI 端鉴权 key |

**逐步迁移建议**:

1. 装 v0.2.0,启动时**不**带任何 `BYTEDANCE_TTS_*` env
2. 浏览器 `/setup`, 把原 env 里的值填到对应字段
3. 验证 `/v1/audio/speech` 正常
4. 下次部署可彻底删 env

**临时兼容**: 老 env 仍可作为 DB 缺失时的 fallback(为 0 重启零配置启动保留),但**不推荐生产用**。

## 资源 ID (重要)

V3 API **只允许两个资源 ID**:

| Resource ID | 说明 |
|---|---|
| `seed-tts-2.0` | 豆包语音合成大模型 2.0 (普通 TTS) |
| `seed-icl-2.0` | 豆包声音复刻大模型 2.0 (复刻 2.0) |

`volc.megatts.icl` / `volc.megatts.default` 等是 **1.0 API** 的资源 / 服务品类编码,**不是 v3 API 2.0 的合法资源 ID**。本项目用复刻 2.0,应填 `seed-icl-2.0`。

复刻音色(speaker 以 `S_` 开头,例如 `S_G8tEKnaJ1`)必须搭配 `seed-icl-2.0`,否则返回 `code=55000000 resource ID is mismatched`。

模型名: `seed-tts-2.0-standard` (这是**复刻 2.0 唯一的子模型名**,**和资源 ID 不同**)。

## 观测 / Metrics

Prometheus 文本格式,无鉴权。可直接被 Prometheus 抓取或浏览器查看。

主要指标:

| 指标 | 类型 | 标签 | 说明 |
|---|---|---|---|
| `tts_request_total` | counter | status, format, speaker, model | /v1/audio/speech 请求数 |
| `tts_request_duration_seconds` | histogram | status, format | 端到端延迟 |
| `tts_upstream_total` | counter | status, format, model, speaker | 上游调用数 |
| `tts_upstream_duration_seconds` | histogram | status, format | 上游调用耗时 |
| `tts_upstream_first_byte_seconds` | histogram | format | TTFB |
| `tts_upstream_chunks_total` | counter | format | 收到的音频 chunk 数 |
| `tts_upstream_audio_bytes_total` | counter | format | 实际返回字节数 |
| `tts_upstream_errors_total` | counter | code | 上游错误 (聚合到 transport/client/server/upstream) |
| `tts_usage_text_words_total` | counter | model | 上游计费字符数 |
| `tts_concurrency_active` | gauge |  | 当前在飞请求数 |
| `tts_concurrency_rejected_total` | counter |  | 并发上限拒绝数 |
| `tts_ratelimit_rejected_total` | counter |  | 速率限制拒绝数 |
| `tts_auth_failed_total` | counter |  | API Key 鉴权失败数 |
| `tts_config_load_failures_total` | counter | mode | TTS 启动配置加载失败数 (告警用) |

**注意**: `speaker` 标签是 `sha1(speaker)[:8]` 哈希值,不是明文,保护火山复刻音色 ID 隐私。

### 告警示例

```yaml
- alert: TTSConfigLoadFailure
  expr: rate(tts_config_load_failures_total{mode="normal"}[5m]) > 0
  for: 1m
  labels: { severity: critical }
  annotations:
    summary: TTS service cannot start due to invalid config
```

## 公网部署安全清单

公网直接暴露 (`:8080` 可被互联网任意访问) 时,**至少满足以下两条之一**:

1. **设置 `OPENAI_TTS_API_KEY`** (推荐, 最简单)
   ```bash
   OPENAI_TTS_API_KEY=<32+ 位随机字符串>
   ```
   客户端请求时带 `Authorization: Bearer <那个字符串>`。

2. **前置反代承担鉴权** (nginx / caddy / Cloudflare Access)
   - 反代层做 basic auth / mTLS / Cloudflare Access 等任一方案
   - 反代**仅**把鉴权后的请求转发到 `:8080`
   - 此时 `OPENAI_TTS_API_KEY` 可不设

**`/metrics` / `/dashboard` / `/health` 均不鉴权**,生产环境务必通过反代保护:

```nginx
location /metrics { allow 10.0.0.0/8; deny all; }  # 仅 Prometheus 服务器
location /dashboard { auth_basic "admin"; auth_basic_user_file /etc/nginx/.htpasswd; }
location /health { allow 10.0.0.0/8; deny all; }  # 或 K8s liveness probe 直接访问
```

## 部署

### Docker

```bash
# .env 至少含 TTS_ADMIN_KEY, 公网再加 OPENAI_TTS_API_KEY
docker compose up -d
```

`docker-compose.yml` 已配 named volume `tts-api-data` 挂载到容器 `/data`,DB 与 lock 文件持久化,容器重启不丢配置。

### Linux Systemd

参见 `v0.1.0 README`,**注意**:v0.2.0 启动前不需要 `EnvironmentFile` 含业务变量,只保留 `TTS_ADMIN_KEY`。

## 架构

| 包 | 职责 |
|---|---|
| `main.go` | 启动入口,模式检测 (setup/normal),fail-fast |
| `installer/` | 启动期模式检测,DB 自愈回退 |
| `setting/` | 全局配置 (TTSOptions / Auth / CORS) + 启动汇总 |
| `store/` | SQLite 数据访问 (settings / voices) + 自愈 |
| `controller/` | /v1/audio/speech、/health、/setup、/admin、/api/setup、/api/settings、/api/voices |
| `middleware/` | SecurityHeaders, CORS, 鉴权, 限流, 并发, 日志, 客户端 IP 提取, install 模式守卫 |
| `router/` | 路由注册 |
| `adapter/volcano/` | 火山 v3 HTTP Chunked 客户端 |
| `telemetry/` | Counter / Gauge / Histogram + Prometheus 文本导出 |
| `metrics/` | TTS 业务指标注册 |
| `cmd/dumpdb/` | ops 工具: dump tts.db |
| `dto/` | 请求/响应类型 |
| `common/` | 常量 + 调试日志 |

## 常见问题

### 1. `code=55000000 resource ID is mismatched`

资源 / 音色不匹配。修复:
1. 火山控制台 → 语音技术 → 你的应用 → 资源管理
2. 用控制台在线体验/调试同一对 `BYTEDANCE_TTS_RESOURCE_ID` + 音色
3. 控制台能合成的组合才是正确的
4. v3 API 复刻 2.0 用 `seed-icl-2.0`, **不要填 `volc.megatts.icl`**
5. 复刻音色 (speaker `S_` 开头) 需确认 Resource ID 已开通

### 2. `code=45000030 requested resource not granted`

账号未开通该资源。控制台 → 资源管理 → 申请开通。

### 3. 资源 ID 该填什么

v3 API 复刻 2.0 项目: **`seed-icl-2.0`** (固定)

`volc.megatts.icl` 是 1.0 服务品类编码,本项目用不了。

### 4. WAV 格式音频播放异常

流式场景下火山 API 的 wav 格式每个 chunk 都返回完整 wav header,拼接后损坏。本项目已自动处理:选择 wav 输出时,内部用 pcm 格式请求 API,本地拼装标准 wav header。如仍有问题,改用 `mp3`。

### 5. PowerShell 下 `curl` 解释错

PowerShell 里 `curl` 是 `Invoke-WebRequest` 的别名。**必须写 `curl.exe`**:

```powershell
curl.exe -X POST "http://localhost:8080/v1/audio/speech" -H "Content-Type: application/json" --data-binary "@body.json"
```

JSON 用单引号包,或写到文件用 `--data-binary "@file.json"`。

### 6. 修改端口

```bash
PORT=8081 ./tts-api
```

或 `.env` 里改 `PORT=8081`。

## 技术支持

如有问题,请检查:
1. 服务启动后日志第一段 "环境配置汇总" — 火山必填项是否全 ✓
2. `/health` 返回 `status: "ok"` 且 `config_error: false`
3. `/admin` → 设置 tab 检查 API Key / 默认资源 ID / 默认音色
4. `/admin` → 音色管理 tab 检查 voice 是否启用
5. 火山控制台 → 在线体验同一对 resource + speaker 能合成
6. 客户端请求 URL 是否以 https:// 开头 (公网)
7. `/admin` → CORS tab 检查白名单含前端完整 origin

## 许可证

本项目采用非商业用途许可协议。详细条款请参阅 [LICENSE](LICENSE) 文件。
