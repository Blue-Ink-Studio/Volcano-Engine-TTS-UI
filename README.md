﻿# 字节跳动火山引擎TTS v3 API 转 OpenAI 兼容接口

## 项目简介

本项目将字节跳动火山引擎TTS（文本转语音）v3 API封装为OpenAI兼容的TTS API接口，使原本调用OpenAI TTS服务的应用可以无缝切换到火山引擎TTS服务。

### 主要特性

- 完全兼容OpenAI `/v1/audio/speech` API接口
- 支持火山引擎TTS v3 API（HTTP Chunked单向流式）
- 支持多种音频格式：mp3、ogg_opus、pcm、wav
- 支持API Key鉴权方式
- 支持多种发音人和模型版本
- 内置速率限制和统计功能
- 支持配置API密钥验证
- 并发限制：最多同时处理10个请求（保护上游API）
- 跨平台支持（Windows/Linux/macOS）

## 快速开始

### 前置要求

- Go 1.19 或更高版本
- 火山引擎账号并开通TTS服务

### 1. 编译程序

```bash
go build -o tts-api .
```

### 2. 配置环境变量

复制 `.env.example` 为 `.env` 并填入你的配置：

```bash
cp .env.example .env
```

编辑 `.env` 文件，填入必要的配置参数。

### 3. 启动服务

```bash
# Windows
tts-api.exe

# Linux/macOS
./tts-api
```

服务默认监听 `8080` 端口。

## 环境变量配置

### 必需参数

| 变量名 | 说明 | 示例 |
|--------|------|------|
| `BYTEDANCE_TTS_API_KEY` | 火山引擎新版控制台 API Key | `your_api_key_here` |
| `BYTEDANCE_TTS_RESOURCE_ID` | 资源ID，决定模型版本 | `seed-tts-1.0` |
| `BYTEDANCE_TTS_SPEAKER` | 发音人（音色）ID | `zh_female_qingxin` |

### 可选参数

| 变量名 | 说明 | 默认值 |
|--------|------|--------|
| `BYTEDANCE_TTS_TIMEOUT` | 请求超时时间 | `30s` |
| `OPENAI_TTS_API_KEY` | OpenAI兼容接口的API密钥（逗号分隔支持多个） | 无 |
| `PORT` | 服务监听端口 | `8080` |
| `ALLOWED_ORIGINS` | 允许跨域请求的来源（多个用英文逗号分隔；调试可设为 `*`） | 无（不设则拒绝所有跨域） |

### Resource ID 说明

| Resource ID | 模型说明 |
|-------------|----------|
| `seed-tts-1.0` | 豆包语音合成模型1.0字符版 |
| `seed-tts-1.0-concurr` | 豆包语音合成模型1.0并发版 |
| `seed-tts-2.0` | 豆包语音合成模型2.0字符版 |
| `seed-icl-1.0` | 声音复刻1.0字符版 |
| `seed-icl-1.0-concurr` | 声音复刻1.0并发版 |
| `seed-icl-2.0` | 声音复刻2.0字符版 |

> 上表为通用模型名。火山控制台实际显示的资源 ID 字符串格式通常是 `volc.megatts.default`、`volc.megatts.icl` 等（带版本号会形如 `volc.megatts.icl.2_0`），**以控制台资源管理页面显示的字符串为准**。资源 ID 与音色必须**同时在控制台开通**才能组合使用，否则 API 会返回 `code=55000000, message=resource ID is mismatched with speaker related resource`。

**注意：** 1.0音色只能搭配 `seed-tts-1.0` Resource ID，2.0音色只能搭配 `seed-tts-2.0` Resource ID。

### 音频格式说明

本项目按参考实现硬编码请求 `wav` / `24000Hz` 格式，HTTP 响应 `Content-Type` 固定为 `audio/wav`。

### v3 API 调用说明

本项目按火山 v3 HTTP Chunked 单向流式 TTS API 实现（[官方文档](https://www.volcengine.com/docs/6561/1598757)），相比 v1/v2 有以下关键差异：

- **协议**：HTTP Chunked 流式，请求路径 `https://openspeech.bytedance.com/api/v3/tts/unidirectional`
- **不再使用业务集群**（`cluster` 字段在 v3 已废弃），改用 `X-Api-Resource-Id` HTTP header 路由模型
- **鉴权 header 只有** `X-Api-Key` 一个，无 `Authorization`，无 app 对象

## CORS 跨域配置

跨域请求由 `ALLOWED_ORIGINS` 环境变量控制，按**完整 origin**（含协议 + 域名 + 端口）精确匹配：

- `https://app.example.com` — 精确匹配一个来源
- `https://a.com,https://b.com` — 多个来源英文逗号分隔
- `*` — 允许所有来源（**不可与凭据请求共存**，需同时去掉 `Authorization` 头）
- `app.example.com` — 缺协议头，**永远不会匹配**（服务端强制校验 `http://` / `https://` 开头）

**典型坑**：

1. 客户端 URL 是 `http://` 但服务端是 `https://`：浏览器按 `http://...` 的 origin 发请求，白名单里的 `https://...` 不会匹配 → 403。**客户端必须用 `https://` 开头**。
2. `ALLOWED_ORIGINS=*` + 客户端带 `Authorization`：浏览器按规范会**直接拒绝预检**（凭据 + 通配符冲突），POST 根本发不出去。
3. 同源请求（前端和 TTS 服务同域名）不受 CORS 限制，`ALLOWED_ORIGINS` 怎么配都不影响。

## API 使用说明

### OpenAI 兼容接口

**端点：** `POST /v1/audio/speech`

**请求头：**
- `Content-Type: application/json`
- `Authorization: Bearer <你的API密钥>`（如果配置了OPENAI_TTS_API_KEY）

**请求体：**
```json
{
  "model": "tts-1",
  "input": "你好，这是一个测试文本",
  "voice": "alloy",
  "response_format": "mp3",
  "speed": 1.0
}
```

**参数说明：**
- `model` - 模型名称（OpenAI兼容，实际不影响）
- `input` - 要合成的文本
- `voice` - 发音人（OpenAI兼容，实际使用配置的BYTEDANCE_TTS_SPEAKER）
- `response_format` - 输出格式：`mp3`（默认）、`opus`（映射到ogg_opus）、`wav`、`pcm`、`aac`/`flac`（降级到mp3）
- `speed` - 语速：0.25 ~ 4.0

**格式映射（OpenAI → 火山）：**

| OpenAI response_format | 火山 API 格式 | Content-Type |
|------------------------|--------------|--------------|
| `mp3` | mp3 | audio/mpeg |
| `opus` | ogg_opus | audio/ogg |
| `wav` | pcm → 封装wav header | audio/wav |
| `pcm` | pcm | audio/pcm |
| `aac` / `flac` | mp3（降级） | audio/mpeg |

**示例调用：**

```bash
# MP3格式（默认）
curl -X POST "http://localhost:8080/v1/audio/speech" \
  -H "Content-Type: application/json" \
  -d '{"model":"tts-1","input":"你好，世界","voice":"alloy","speed":1.0}' \
  -o output.mp3

# WAV格式
curl -X POST "http://localhost:8080/v1/audio/speech" \
  -H "Content-Type: application/json" \
  -d '{"model":"tts-1","input":"你好，世界","voice":"alloy","response_format":"wav","speed":1.0}' \
  -o output.wav
```

### 健康检查（含统计信息）

```bash
curl http://localhost:8080/health
```

返回包含：服务状态、请求统计、错误记录、配置检查结果

## 限流机制

为保护上游火山引擎API，服务实现了两层限流保护：

### 1. 全局并发限制
- **限制**：最多同时处理 **10个** TTS请求
- **触发**：超过10个并发请求时
- **错误码**：`503 Service Unavailable`
- **说明**：确保不超过上游API的并发限制

### 2. IP速率限制
- **限制**：每个IP每分钟 **100个** 请求
- **触发**：单个IP调用过于频繁
- **错误码**：`429 Too Many Requests`
- **说明**：防止单个客户端滥用服务

### 触发限流时的响应
```json
{
  "error": {
    "message": "Server is busy, maximum concurrent requests reached.",
    "type": "concurrency_limit_error",
    "code": "max_concurrent_requests"
  }
}
```

### 服务器日志
触发限流时服务器会输出中文警告日志：
- `警告: 已达到最大并发请求数限制，拒绝请求 - 客户端IP: x.x.x.x`
- `警告: 已超过IP速率限制，拒绝请求 - 客户端IP: x.x.x.x`

## 支持的发音人

具体发音人列表请参考火山引擎官方文档：
- 1.0音色：https://www.volcengine.com/docs/6561/97454
- 2.0音色：https://www.volcengine.com/docs/6561/1340515

## 常见问题

### 1. 如何获取鉴权信息？

- 登录火山引擎新版控制台
- 进入"语音合成"服务
- 创建应用并获取API Key

### 2. 端口被占用怎么办？

通过环境变量修改端口：

```bash
# Windows
set PORT=8081 && tts-api.exe

# Linux/macOS
PORT=8081 ./tts-api
```

### 3. 如何配置多个API密钥？

使用逗号分隔：

```bash
OPENAI_TTS_API_KEY=sk-key1,sk-key2,sk-key3
```

### 4. 出现 `code=55000000, message=resource ID is mismatched with speaker related resource` 怎么办？

这是火山引擎 v3 API 返回的**资源/音色不匹配**错误，不是网络或超时问题。修复方法：

1. 去**火山控制台** → 语音技术 → 你的应用 → 资源管理或音色库
2. 用控制台的在线体验/调试试一下同一对 `BYTEDANCE_TTS_RESOURCE_ID` + 音色
3. 控制台能合成的组合才是正确的
4. 把控制台显示的**实际资源 ID 字符串**（通常是 `volc.megatts.*` 格式）填到 `BYTEDANCE_TTS_RESOURCE_ID`
5. 如果你用的是**声音复刻**音色（speaker 以 `S_` 开头），确认 `BYTEDANCE_TTS_RESOURCE_ID` 已在火山控制台开通，并且和音色 ID 在同一个资源族下。55000000 通常是资源族不匹配

### 5. PowerShell 下 `curl` 命令被解释错

PowerShell 里 `curl` 是 `Invoke-WebRequest` 的别名，参数完全不同。**必须写 `curl.exe`**：

```powershell
curl.exe -v -X POST "http://localhost:8080/v1/audio/speech" -H "Content-Type: application/json" -H "Authorization: Bearer YOUR_KEY" --data-binary "@body.json"
```

另外 PowerShell 里 `{"foo":"bar"}` 不加单引号会被当成脚本块解析。**要么用单引号包 JSON**，要么把 body 写到文件用 `--data-binary "@file.json"`。

### 6. WAV 格式音频播放异常？

流式场景下火山 API 的 wav 格式会每个 chunk 都返回完整的 wav header，拼接后音频损坏。本项目已自动处理：选择 wav 输出时，内部用 pcm 格式请求 API，最后拼装标准 wav header。如仍有问题，建议改用 `mp3` 格式。

### 7. 查看日志

服务启动后输出到 stdout/stderr。常见日志关键字：

**中间件层拒绝**（有专门日志）：

```
CORS拦截: 来源="https://..." 路径=/v1/audio/speech 方法=POST 客户端=...
警告: 已超过IP速率限制，拒绝请求 - 客户端IP: 1.2.3.4
警告: 已达到最大并发请求数限制，拒绝请求 - 客户端IP: 1.2.3.4
```

**Controller 层拒绝**（每条都带具体原因和客户端 IP）：

```
警告: 错误的方法 - 方法=GET 期望=POST 路径=/v1/audio/speech 客户端=...
警告: API Key 鉴权失败 - 路径=/v1/audio/speech 客户端=... 远端=...
警告: TTS配置未就绪，拒绝请求 - 错误=缺少必需的环境变量: [BYTEDANCE_TTS_API_KEY] 路径=...
警告: 请求体过大 - 路径=... 限制=1048576字节
警告: 读取请求体失败 - 路径=... 错误=...
警告: JSON 解析失败 - 路径=... 错误=... body前200字节="..."
警告: Model 名过长 - 路径=... 长度=80 限制=64
警告: Model 名含非法字符 - 路径=... model前50字节="..."
警告: input 字段为空 - 路径=...
警告: input 文本过长 - 路径=... 长度=6000 限制=5000
警告: TTS 合成失败 - 路径=... 文本长度=50 耗时=114ms 错误=...
```

**适配器层日志**：

```
Sentence start: sequence=0, sentence=...
Sentence end: sequence=0
TTS synthesis completed, usage: &{TextWords:5}
```

**请求结束通用日志**（每个请求都有，由 Logger 中间件输出）：

```
POST /v1/audio/speech 1.2.3.4:56789 200 245ms
POST /v1/audio/speech 1.2.3.4:56789 400 1ms
```


## 观测 / Metrics

服务内置 Prometheus 文本格式的 `/metrics` 端点,**不鉴权**(与 `/health` 一致),
可直接被 Prometheus 抓取或浏览器查看。Go 进程内埋点,零外部依赖,实现位于 `telemetry/` 与 `metrics/` 包。

### 主要指标

| 指标名 | 类型 | 标签 | 说明 |
|---|---|---|---|
| `tts_request_total` | counter | status, format, speaker, model | /v1/audio/speech 请求数 |
| `tts_request_duration_seconds` | histogram | status, format | 端到端延迟 |
| `tts_upstream_total` | counter | status, format, model, speaker | 上游调用数 |
| `tts_upstream_duration_seconds` | histogram | status, format | 上游调用耗时 |
| `tts_upstream_first_byte_seconds` | histogram | format | TTFB |
| `tts_upstream_chunks_total` | counter | format | 收到的音频 chunk 数 |
| `tts_upstream_audio_bytes_total` | counter | format | 实际返回字节数 |
| `tts_upstream_errors_total` | counter | code | 上游错误(code 聚合到 transport/client/server/upstream) |
| `tts_usage_text_words_total` | counter | model | 上游计费字符数 |
| `tts_concurrency_active` | gauge |  | 当前在飞请求数 |
| `tts_concurrency_rejected_total` | counter |  | 并发上限拒绝数 |
| `tts_ratelimit_rejected_total` | counter |  | 速率限制拒绝数 |
| `tts_auth_failed_total` | counter |  | API Key 鉴权失败数 |

### Prometheus 抓取示例

```yaml
scrape_configs:
  - job_name: tts-api
    static_configs:
      - targets: ['localhost:8080']
```

### 仪表盘

`/dashboard` 仍展示服务状态 + 内存 + 配置信息,并内嵌 `/metrics` 的预览;
Grafana 等工具可直接基于上面指标做面板。

## 架构

| 包 | 职责 |
|---|---|
| `main.go` | 启动入口,信号处理 |
| `telemetry/` | Counter / Gauge / Histogram + Prometheus 文本导出(零依赖) |
| `metrics/` | TTS 业务指标注册,火山适配器埋点适配 |
| `adapter/volcano/` | 火山 v3 HTTP Chunked 客户端(client/request/response/audio/errors/synthesis) |
| `controller/` | /v1/audio/speech、/health 处理器 |
| `middleware/` | SecurityHeaders、CORS、鉴权、限流、并发、日志、客户端 IP 提取 |
| `setting/` | 单一环境变量入口 + 启动汇总 |
| `common/`、`dto/` | 常量、请求/响应类型 |
| `router/` | 路由注册 |

## 部署建议

### Linux Systemd 服务

创建 `/etc/systemd/system/tts-server.service`：

```ini
[Unit]
Description=ByteDance TTS to OpenAI API Adapter
After=network.target

[Service]
Type=simple
User=www-data
WorkingDirectory=/www/wwwroot/tts-server
EnvironmentFile=/www/wwwroot/tts-server/.env
ExecStart=/www/wwwroot/tts-server/tts-api
Restart=always
RestartSec=10

[Install]
WantedBy=multi-user.target
```

启动服务：

```bash
sudo systemctl daemon-reload
sudo systemctl enable tts-server
sudo systemctl start tts-server
```

### Docker 部署

```bash
docker compose up -d
```

环境变量通过 `.env` 文件或 docker-compose.yml 传入。

## 许可证

本项目采用非商业用途许可协议。您可以免费使用本软件用于非商业目的，但禁止用于任何商业活动。详细条款请参阅 [LICENSE](LICENSE) 文件。

## 技术支持

如有问题，请检查：
1. 环境变量配置是否正确
2. 网络是否能访问火山引擎TTS服务
3. 鉴权信息是否有效
4. Resource ID与Speaker是否匹配
5. ALLOWED_ORIGINS 是否包含前端完整 origin（含 https://）
6. 客户端请求 URL 是否以 https:// 开头
7. 生产环境凭据是否定期轮换（API Key 明文出现在日志/对话中时立刻重置）
8. 复刻音色（speaker 以 `S_` 开头）确保 `BYTEDANCE_TTS_RESOURCE_ID` 在控制台和音色 ID 同族（参考 `seed-icl-2.0` 等表）
9. 音频格式是否匹配客户端解码能力（默认 mp3 兼容性最好）
