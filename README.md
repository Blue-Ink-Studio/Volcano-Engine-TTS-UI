# 字节跳动火山引擎TTS v3 API 转 OpenAI 兼容接口

## 项目简介

本项目将字节跳动火山引擎TTS（文本转语音）v3 API 封装为 OpenAI 兼容的 TTS API 接口,使原本调用 OpenAI TTS 服务的应用可以无缝切换到火山引擎。

### 主要特性

- 完全兼容 OpenAI `/v1/audio/speech` API
- 支持火山引擎 TTS v3 HTTP Chunked 单向流式 API
- 支持多种音频格式:mp3 / ogg_opus / pcm / wav(wav 内部转 pcm 后本地拼头)
- 支持火山复刻 2.0 子模型(`seed-tts-2.0-standard` / `-expressive`)
- API Key 鉴权、IP 速率限制、全局并发限制
- 内置 Prometheus 文本格式 `/metrics` 端点,零外部依赖
- 跨平台支持(Windows / Linux / macOS)

## 快速开始

### 前置要求

- Go 1.26 或更高版本
- 火山引擎账号并开通 TTS 服务

### 1. 编译

```bash
go build -o tts-api .
```

### 2. 配置环境变量

复制 `.env.example` 为 `.env` 并填入实际配置:

```bash
cp .env.example .env
```

### 3. 启动

```bash
# Windows
tts-api.exe

# Linux/macOS
./tts-api
```

服务默认监听 `8080` 端口,可通过 `PORT` 环境变量修改。

## 环境变量配置

### 必需参数

| 变量名 | 说明 |
|--------|------|
| `BYTEDANCE_TTS_API_KEY` | 火山引擎新版控制台 API Key |
| `BYTEDANCE_TTS_RESOURCE_ID` | 资源 ID,决定模型版本与计费(`seed-tts-1.0` / `seed-icl-2.0` 等) |
| `BYTEDANCE_TTS_SPEAKER` | 发音人(音色)ID,复刻音色以 `S_` 开头 |

### TTS 行为参数

| 变量名 | 说明 | 默认值 |
|--------|------|--------|
| `BYTEDANCE_TTS_TIMEOUT` | 单次合成超时 | `30s` |
| `BYTEDANCE_TTS_FORMAT` | 上游实际请求的音频格式(mp3 / pcm / ogg_opus);客户端要求 wav 时内部自动转 pcm + 本地拼 WAV 头 | `mp3` |
| `BYTEDANCE_TTS_SAMPLE_RATE` | 上游采样率(8000 / 16000 / 22050 / 24000 / 32000 / 44100 / 48000) | `24000` |
| `BYTEDANCE_TTS_BIT_RATE` | MP3 比特率,仅 mp3 生效 | 无 |

### 复刻 2.0 扩展参数

| 变量名 | 说明 | 默认值 |
|--------|------|--------|
| `BYTEDANCE_TTS_MODEL` | 复刻 2.0 子模型(`seed-tts-2.0-standard` / `seed-tts-2.0-expressive`) | 控制台默认 |
| `BYTEDANCE_TTS_MODEL_TYPE` | 模型类型(4=ICL V2, 5=ICL V3),推荐显式指定 | 无 |
| `BYTEDANCE_TTS_EXPLICIT_LANGUAGE` | 非中英文合成时指定语种(zh-cn / en / ja / es-mx / id / pt-br / ko) | 无 |
| `BYTEDANCE_TTS_ENABLE_SUBTITLE` | 启用字级时间戳(复刻 2.0 生效) | `false` |

### 运行时 / 服务参数

| 变量名 | 说明 | 默认值 |
|--------|------|--------|
| `OPENAI_TTS_API_KEY` | 🔴 **公网必设** OpenAI 兼容接口的 API Key(逗号分隔支持多个);**未设置时鉴权完全关闭** | 无(不鉴权) |
| `TRUSTED_PROXY_HOPS` | X-Forwarded-For 解析模式(0=启发式/默认,>0=精确 N 跳) | `0`(启发式) |
| `PORT` | 服务监听端口 | `8080` |
| `ALLOWED_ORIGINS` | CORS 跨域白名单(逗号分隔,调试可设 `*`;空则拒绝所有跨域) | 无 |

### 反代拓扑与 X-Forwarded-For 解析

当服务部署在反代(nginx / caddy / CDN)后面时,反代会通过 `X-Forwarded-For`(XFF)头传递真实客户端 IP。本服务通过 `TRUSTED_PROXY_HOPS` 环境变量控制 XFF 解析方式,支持两种模式。

#### 何时需要关心这个配置

| 部署方式 | 是否需要配置 |
|---|---|
| 服务直接暴露公网 IP(无反代)| ❌ 不适用,跳过本节 |
| 服务前有 1 个反代(nginx / caddy)| ❌ 不必配置,启发式模式自动处理 |
| 服务前有 2 跳以上反代(CDN + 自建反代)| ⚠️ 启发式模式"够用",需要精准按真实 client 限流时再设 |

> **直出部署(无反代)的用户**:本节不适用,跳过阅读。`TRUSTED_PROXY_HOPS` 在你的部署下不会被读取。

#### 启发式模式(默认 / `TRUSTED_PROXY_HOPS=0`)

从 XFF 链尾向前扫描,**跳过私有 IP,返回第一个公网 IP**。

适用场景:单跳反代(最常见)、多跳含公网代理(CDN + nginx)。

**行为示例**:

| XFF 链 | 启发式返回 | 备注 |
|---|---|---|
| `1.2.3.4` | `1.2.3.4` | 单跳,真实 client |
| `fake, 1.2.3.4` | `1.2.3.4` | 攻击者伪造首值,跳过 fake |
| `1.2.3.4, 5.6.7.8, 10.0.0.1` | `5.6.7.8` | 多跳,返回最末公网 IP(CDN 边缘) |
| `1.2.3.4, 192.168.1.1` | `1.2.3.4` | 链尾是私有 IP,跳过 |

**优点**:零配置,大多数部署自动正确。

**限制**:多跳 CDN 场景下,限流粒度为"按 CDN 边缘 IP"而非"按真实 client"。攻击者填满某 CDN 边缘配额可能影响该 CDN 下的其他用户——但无法伪造身份、无法越权。

#### 精确模式(`TRUSTED_PROXY_HOPS=N`,N > 0)

从 XFF 链尾倒数第 N+1 个位置取值,即"信任最近 N 跳反代,取该信任链之前那一跳的 IP"。

适用场景:多跳 CDN + 反代,且需要精准按真实 client 限流。

**N 的确定方法**:统计客户端到本服务之间的反代跳数。

| 拓扑 | 跳数 | 配置 |
|---|---|---|
| `client → nginx → 本服务` | 1 | `TRUSTED_PROXY_HOPS=1` |
| `client → Cloudflare → nginx → 本服务` | 2 | `TRUSTED_PROXY_HOPS=2` |
| `client → CDN → WAF → nginx → 本服务` | 3 | `TRUSTED_PROXY_HOPS=3` |

**行为对比**(以 `client(1.2.3.4) → CDN(203.0.113.5) → nginx(10.0.0.1) → 本服务` 为例,XFF 链 = `1.2.3.4, 203.0.113.5`):

| `TRUSTED_PROXY_HOPS` | 返回 | 评价 |
|---|---|---|
| 0(默认启发式)| `203.0.113.5` | CDN 边缘 IP,限流粒度粗 |
| 1(数到 nginx,未穿透)| `203.0.113.5` | 配置不当,与默认相同 |
| 2(穿透到真实 client)| `1.2.3.4` | 精准到真实 client ✓ |
| 3(超出实际跳数)| `directIP`(链长不足保护)| 配置错误,需修正 |

#### 为什么两种模式都从链尾扫描

XFF 链的第一个值是**客户端可控**的:攻击者可以发送任意 `X-Forwarded-For: 1.2.3.4`,若反代用追加模式(如 nginx 默认的 `$proxy_add_x_forwarded_for`),链尾才会追加真实 IP。

若代码取首值,攻击者每次换伪造 IP 即可绕过 IP 限流,也可伪装成受害 IP 把其配额耗尽(间接 DoS)。两种模式都从链尾扫描,天然免疫这种攻击。

#### 验证当前模式

启动期日志会显示当前模式:

```
TRUSTED_PROXY_HOPS 未设置,使用默认启发式模式(XFF 链尾第一个公网 IP)
# 或
已配置 TRUSTED_PROXY_HOPS=0(启发式模式,等同默认)
# 或
已配置 TRUSTED_PROXY_HOPS=2(精确模式,信任 2 跳反代)
```

也可在 `GetClientIP` 临时加 `log.Printf` 打印解析结果,或写一个 Go 测试用例(参见 DEBT-1 单元测试任务)来覆盖不同 XFF 链场景。生产环境不要保留 debug 日志。

### Resource ID 说明

| Resource ID | 模型说明 |
|-------------|----------|
| `seed-tts-1.0` | 豆包语音合成模型 1.0 字符版 |
| `seed-tts-1.0-concurr` | 豆包语音合成模型 1.0 并发版 |
| `seed-tts-2.0` | 豆包语音合成模型 2.0 字符版 |
| `seed-icl-2.0` | 声音复刻 2.0 字符版 |

> 上表为通用模型名。火山控制台实际显示的资源 ID 字符串通常是 `volc.megatts.default`、`volc.megatts.icl` 等(带版本号形如 `volc.megatts.icl.2_0`),**以控制台资源管理页面显示的字符串为准**。资源 ID 与音色必须**同时在控制台开通**才能组合使用,否则 API 返回 `code=55000000, message=resource ID is mismatched with speaker related resource`。

**注意:** 复刻音色(speaker 以 `S_` 开头)必须搭配对应族的 Resource ID,否则 API 返回 resource mismatched 错误。

## 调试日志

### BYTEDANCE_TTS_DEBUG

服务运行期日志分为**始终输出**和**调试模式才输出**两类。通过 `BYTEDANCE_TTS_DEBUG` 环境变量控制调试日志开关。

| 值 | 行为 |
|----|------|
| 不设置 / `false` | 仅输出错误、警告、启动摘要、成功日志(默认,生产环境推荐) |
| `true` | 额外输出适配器层调试日志 |

```bash
# 启用调试
BYTEDANCE_TTS_DEBUG=true ./tts-api

# 或写入 .env
echo "BYTEDANCE_TTS_DEBUG=true" >> .env
```

启用后启动时会打印:

```
调试日志已启用 BYTEDANCE_TTS_DEBUG
```

### 始终输出的日志

启动摘要、错误警告、合成成功/失败、访问日志(Logger 中间件):

```
[TTS-Server] config.go:238: === 环境配置汇总 ===
[TTS-Server] config.go:239: 服务端口: 8080
...
警告: TTS 合成失败 - 路径=/v1/audio/speech 客户端=... 文本长度=50 耗时=114ms 错误=...
TTS 合成成功 - 音色=zh_female_qingxin 格式=mp3 文本=50字 音频=12345字节 分片=3 耗时=1.2s
POST /v1/audio/speech 1.2.3.4:56789 200 1.2s
```

### 调试模式才输出的日志(`BYTEDANCE_TTS_DEBUG=true`)

适配器层与 CORS 拦截详情:

```
TTS upstream: resource_id=seed-icl-2.0 speaker=zh_female_qingxin model="seed-tts-2.0-standard" format=mp3 sample_rate=24000 speech_rate=0 additions="..."
Sentence start: sequence=0, sentence=...
Sentence end: sequence=0
TTS 合成结束, usage: text_words=5
volcano: 忽略未识别事件 event="xxx" sequence=1
CORS拦截: 来源="https://..." 路径=/v1/audio/speech 方法=POST 客户端=...
```

> **生产建议:** 默认不开 `BYTEDANCE_TTS_DEBUG`,需要排查问题时再临时开启,避免 sentence 级别日志刷屏。

## CORS 跨域配置

跨域请求由 `ALLOWED_ORIGINS` 控制,按**完整 origin**(协议 + 域名 + 端口)精确匹配:

- `https://app.example.com` — 精确匹配一个来源
- `https://a.com,https://b.com` — 多个来源逗号分隔
- `*` — 允许所有来源(**不可与凭据请求共存**)
- `app.example.com` — 缺协议头,**永远不会匹配**(强制校验 `http://` / `https://` 开头)

**典型坑:**

1. 客户端是 `http://` 但服务端是 `https://`:浏览器按 `http://...` 的 origin 发请求,白名单里的 `https://...` 不会匹配 → 403。**客户端必须用 `https://` 开头**。
2. `ALLOWED_ORIGINS=*` + 客户端带 `Authorization`:浏览器按规范**直接拒绝预检**(凭据 + 通配符冲突),POST 根本发不出去。
3. 同源请求不受 CORS 限制。

## API 使用说明

> ⚠️ **公网部署前必读**:如果你的服务暴露在公网,**必须**设置 `OPENAI_TTS_API_KEY` 或由前置反代(nginx / caddy)承担鉴权。未设置时 `Authorization` 头完全跳过校验,任何能访问 `:8080` 的人都能调用 TTS 合成,消耗你的火山额度。详见[部署 → 公网安全清单](#公网部署安全清单)。

### OpenAI 兼容接口

**端点:** `POST /v1/audio/speech`

**请求头:**
- `Content-Type: application/json`
- `Authorization: Bearer <你的API密钥>`(如果配置了 `OPENAI_TTS_API_KEY`)

**请求体:**

```json
{
  "model": "tts-1",
  "input": "你好,这是一个测试文本",
  "voice": "alloy",
  "response_format": "mp3",
  "speed": 1.0
}
```

**参数说明:**
- `model` — 模型名(OpenAI 兼容,实际不影响,火山侧用 `BYTEDANCE_TTS_MODEL`)
- `input` — 要合成的文本
- `voice` — 发音人(OpenAI 兼容,实际用 `BYTEDANCE_TTS_SPEAKER`)
- `response_format` — 输出格式:`mp3`(默认)/ `opus`(映射 ogg_opus)/ `wav` / `pcm` / `aac` / `flac`(降级到 mp3)
- `speed` — 语速,0.25 ~ 4.0(火山侧转换为 speech_rate [-50, 100])

**格式映射:**

| OpenAI response_format | 火山 API 格式 | Content-Type |
|------------------------|--------------|--------------|
| `mp3` | mp3 | audio/mpeg |
| `opus` | ogg_opus | audio/ogg |
| `wav` | pcm → 本地拼 wav header | audio/wav |
| `pcm` | pcm | audio/pcm |
| `aac` / `flac` | mp3(降级) | audio/mpeg |

**调用示例:**

```bash
# MP3
curl -X POST "http://localhost:8080/v1/audio/speech" \
  -H "Content-Type: application/json" \
  -d '{"model":"tts-1","input":"你好,世界","voice":"alloy","speed":1.0}' \
  -o output.mp3

# WAV
curl -X POST "http://localhost:8080/v1/audio/speech" \
  -H "Content-Type: application/json" \
  -d '{"model":"tts-1","input":"你好,世界","voice":"alloy","response_format":"wav"}' \
  -o output.wav
```

### 健康检查

```bash
curl http://localhost:8080/health
```

返回服务状态、版本、运行时长、内存、配置检查结果(**不鉴权**)。

## 限流机制

为保护上游火山 API,服务实现两层限流:

### 全局并发限制
- 最多同时处理 **10 个** TTS 请求
- 超过返回 `503 Service Unavailable`

### IP 速率限制
- 每个 IP 每分钟 **100 个** 请求
- 超过返回 `429 Too Many Requests`

**触发日志(始终输出):**

```
警告: 已达到最大并发请求数限制,拒绝请求 - 客户端IP: 1.2.3.4
警告: 已超过IP速率限制,拒绝请求 - 客户端IP: 1.2.3.4
```

## 观测 / Metrics

服务内置 Prometheus 文本格式的 `/metrics` 端点,**不鉴权**(与 `/health` 一致),可直接被 Prometheus 抓取或浏览器查看。Go 进程内埋点,零外部依赖,实现位于 `telemetry/` 与 `metrics/` 包。

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

`/dashboard` 展示服务状态 + 内存 + 配置信息,并内嵌 `/metrics` 预览;Grafana 等工具可直接基于上面指标做面板。

### ⚠️ 公网部署:监控端点无鉴权

`/metrics`、`/health`、`/dashboard` **均不鉴权**,这是对齐 Prometheus 抓取场景的设计权衡:

| 端点 | 暴露内容 | 风险 |
|---|---|---|
| `/metrics` | 业务标签(speaker/model/format)、运行指标、错误计数 | 侦察面:可推断使用量、技术栈、错误模式 |
| `/health` | 服务状态、版本号、运行时长、内存 | 侦察面:版本号可用于匹配已知 CVE |
| `/dashboard` | 配置检查结果(含 `TTSConfigErr` 状态) | 信息泄露:可确认配置是否就绪 |

**部署建议**:

- **内网 / 反代后**:无影响,符合预期
- **公网直接暴露**:在前置反代(nginx / caddy)上保护这些端点,示例 nginx 配置:

  ```nginx
  location /metrics {
      auth_basic "metrics";
      auth_basic_user_file /etc/nginx/.htpasswd;
      allow 10.0.0.0/8;        # 仅允许 Prometheus 服务器网段
      deny all;
  }
  location /dashboard {
      auth_basic "admin";
      auth_basic_user_file /etc/nginx/.htpasswd;
  }
  location /health {
      allow 10.0.0.0/8;        # 或保留给监控系统访问
      deny all;
  }
  ```

- **最简方案**:反代层直接限制 `/metrics` 只能从 Prometheus 服务器 IP 访问,无需 basic auth

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
| `common/`、`dto/` | 常量、请求/响应类型,`common.DebugLog` 控制调试日志 |
| `router/` | 路由注册 |

## 部署

### Linux Systemd

创建 `/etc/systemd/system/tts-server.service`:

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

```bash
sudo systemctl daemon-reload
sudo systemctl enable tts-server
sudo systemctl start tts-server
```

### Docker

```bash
docker compose up -d
```

环境变量通过 `.env` 或 `docker-compose.yml` 传入。

### 公网部署安全清单

公网直接暴露(`:8080` 可被互联网任意访问)时,**至少满足以下两条之一**,否则视为不安全的部署:

1. **设置 `OPENAI_TTS_API_KEY`**(推荐,最简单)
   ```bash
   # .env
   OPENAI_TTS_API_KEY=<32+ 位随机字符串>
   ```
   客户端请求时带 `Authorization: Bearer <那个字符串>`。

2. **前置反代承担鉴权**(nginx / caddy / Cloudflare Access)
   - 反代层做 basic auth、mTLS、Cloudflare Access 等任一方案
   - 反代**仅**把鉴权后的请求转发到 `:8080`,Go 服务本身保持"无鉴权"
   - 此时 `OPENAI_TTS_API_KEY` 可不设

**两个端点还需要单独保护**(无论上面哪种方案):

- `/metrics`:暴露业务标签与运行指标,详见[观测 / Metrics → 公网部署](#公网部署监控端点无鉴权)
- `/dashboard`:暴露配置检查结果,同上

**未做保护的典型风险**:
- 任意人 curl `POST /v1/audio/speech` → 消耗你火山账号的字符额度
- 任意人 `GET /metrics` → 推断你的使用量、技术栈、错误模式
- 任意人 `GET /dashboard` → 确认你 TTS 配置就绪状态

**内网部署 / 私网反代后**:这些警示不适用,直接用就行。

## 常见问题

### 1. `code=55000000, message=resource ID is mismatched with speaker related resource`

资源/音色不匹配。修复:

1. 火山控制台 → 语音技术 → 你的应用 → 资源管理或音色库
2. 用控制台在线体验/调试同一对 `BYTEDANCE_TTS_RESOURCE_ID` + 音色
3. 控制台能合成的组合才是正确的
4. 把控制台实际显示的资源 ID 字符串(通常是 `volc.megatts.*` 格式)填到 `BYTEDANCE_TTS_RESOURCE_ID`
5. 复刻音色(speaker 以 `S_` 开头)需确认 Resource ID 已开通且与音色同族

### 2. PowerShell 下 `curl` 解释错

PowerShell 里 `curl` 是 `Invoke-WebRequest` 的别名。**必须写 `curl.exe`**:

```powershell
curl.exe -v -X POST "http://localhost:8080/v1/audio/speech" -H "Content-Type: application/json" --data-binary "@body.json"
```

JSON 用单引号包,或写到文件用 `--data-binary "@file.json"`。

### 3. WAV 格式音频播放异常

流式场景下火山 API 的 wav 格式每个 chunk 都返回完整 wav header,拼接后损坏。本项目已自动处理:选择 wav 输出时,内部用 pcm 格式请求 API,本地拼装标准 wav header。如仍有问题,改用 `mp3`。

### 4. 调试时如何看详细日志

设置 `BYTEDANCE_TTS_DEBUG=true` 后重启服务,会额外输出上游请求参数、sentence 事件、CORS 拦截等。详见上文「调试日志」一节。

### 5. 多 API Key 配置

```bash
OPENAI_TTS_API_KEY=sk-key1,sk-key2,sk-key3
```

### 6. 修改端口

```bash
PORT=8081 ./tts-api
```

## 技术支持

如有问题,请检查:

1. 环境变量配置是否正确
2. 网络是否能访问火山引擎 TTS 服务
3. 鉴权信息是否有效
4. Resource ID 与 Speaker 是否匹配
5. `ALLOWED_ORIGINS` 是否包含前端完整 origin(含 https://)
6. 客户端请求 URL 是否以 https:// 开头
7. 生产环境凭据是否定期轮换
8. 复刻音色确保 Resource ID 与音色 ID 同族
9. 音频格式是否匹配客户端解码能力(默认 mp3 兼容性最好)

## 许可证

本项目采用非商业用途许可协议。详细条款请参阅 [LICENSE](LICENSE) 文件。
