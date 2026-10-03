# 上游适配器开发指南

> 面向对象:要为本项目接入**新上游 TTS 服务**的开发者(也包括未来的你)。
> 阅读前提:知道 OpenAI `/v1/audio/speech` 的请求/响应形状,能读 Go。
>
> ⚠️ **本文档分两部分,请先看清区别**:
> **第一部分「现状」描述的是代码里已经存在的东西**,可以直接照着读代码。
> **第二部分「目标架构」是待实施的设计**,代码里**还不存在** ——
> 里面的接口、表结构、目录都是提案,需要先按 §4 的分阶段计划落地。
> 不要把第二部分的接口名当成现有 API 去调用。

---

## 1. 愿景与现状

### 1.1 愿景

把本服务做成**多渠道多上游聚合层**:对外始终是一个 OpenAI 兼容接口,
对内可以挂火山、OpenAI、Azure、阿里云、自建 GPT-SoVITS / IndexTTS……等任意上游。
调用方不需要知道音频是哪个厂商合成的 —— 换上游对客户端应当**透明**。

### 1.2 现状(第一部分)

目前**只有火山一个适配器**,且不是"插件式"的,而是**硬编码进主干**的。
这一点必须说清楚,因为它决定了新上游不能"只写一个文件就完事"。

```
入口         controller/tts.go        OpenaiTTSHandler
                 │
                 │  opts := setting.GetTTSOptions()      ← 类型是 volcano.Options
                 │  volcano.Synthesis(ctx, volcanoClient, opts, ...)   ← 直接调实现,无接口
                 ▼
适配器       adapter/volcano/         Options / Synthesis / ParseStream / UpstreamError
                 ▲
                 │  setting.LoadRuntimeConfig 直接构造 volcano.Options / volcano.Additions
配置         setting/config.go
```

**关键事实**:主干里有一处 `import "…/adapter/volcano"`,不是注解式的,
而是**类型级耦合**。所以"多上游"不是加文件,而是一次重构(见 §4)。

当前只有火山适配器时,以下代码是合理的;接第二个上游时必须逐个处理,
否则会出现"配置是火山的、路由是 OpenAI 的"这种错配。

---

## 2. 现有火山适配器契约(照此实现即可)

新适配器要能替代火山适配器,就必须满足下面这些**已经被主干依赖**的契约。
先读这一节,再读 §3 的接口设计。

### 2.1 包的职责划分

| 文件 | 职责 | 新适配器对应物 |
|---|---|---|
| `adapter/volcano/options.go` | 调用参数集合(`Options`)+ 上游扩展参数(`Additions`)+ `IsZero()` | provider 私有 opts |
| `adapter/volcano/request.go` | `Options` → 上游请求体;`buildRequest` 做必填校验;`convertSpeedToSpeechRate` 做 OpenAI speed 换算 | 请求构造 |
| `adapter/volcano/client.go` | 复用连接的 `*HTTPClient`(keep-alive 调优);`PostStream` 发流式请求 | 传输层 |
| `adapter/volcano/synthesis.go` | **唯一入口** `Synthesis(...)`;编排 埋点 → 构造 → 发送 → 解析 → 收尾 | `Synthesize(...)` |
| `adapter/volcano/response.go` | `ParseStream` 解析 NDJSON 流;`ParsedStream` 累计结果 | 响应解析 |
| `adapter/volcano/errors.go` | `UpstreamError{Code,Message,Stage,Wrapped}` + `IsAuth()` | 错误类型 |
| `adapter/volcano/audio.go` | `WrapWAVHeader` —— 上游只给 PCM 时本地拼标准 wav 头 | 格式收尾 |

### 2.2 主干对适配器的硬性依赖(必须逐条满足)

| # | 主干行为 | 位置 | 新适配器必须 |
|---|---|---|---|
| 1 | 用包级 `*HTTPClient` 发请求,连接复用 | `controller/tts.go:27,32` | 提供可复用的 client,不要每次 new |
| 2 | 可用 `ctx` 控制超时 | `controller/tts.go:203` | `Synthesize` 必须接受 `context.Context` |
| 3 | 埋点接口 **3 个方法** | `adapter/volcano/synthesis.go:20-24` | 见 §2.3 |
| 4 | 错误按 `Stage` 分类成 status label | `controller/tts.go:242-256` | 错误必须携带阶段:`request`/`http`/`stream`/`wrap` |
| 5 | 错误码进 metrics label | `metrics/metrics.go:160-172` | 非零 code 会被聚合成 `client`/`server`/`upstream` |
| 6 | 返回结果结构 | `dto.SynthesisResult` | 见 §2.4 |
| 7 | 上游不支持的输出格式要**降级** | `synthesis.go:63-65`、`README` 格式表 | 见 §2.5 |

### 2.3 埋点接口(MetricsRecorder)

适配器**不 import telemetry 包**,而是由主干注入一个接口实现
(`controller/tts.go:28` 注入 `metrics.AdapterRecorder{}`)。这是刻意的解耦,新适配器请沿用。

```go
type MetricsRecorder interface {
    UpstreamStarted(speaker, model, format string)
    UpstreamFinished(speaker, model, format, status string,
        duration, ttfb time.Duration, chunks, audioBytes, errCode int)
    UpstreamUsage(model string, textWords int)
}
```

现状实现里 `speaker` 是**语料无关的上游音色 ID**,会被 `telemetry.SpeakerLabel`
做 `sha1[:8]` 哈希后才当 label(隐私保护,见 `metrics/metrics.go:127`)。
新适配器传自己的音色标识即可,主干不解释它的含义。

`status` 的取值约定(沿用即可,聚合看板依赖它们):
`ok` / `request_error` / `transport_error` / `http_<code>` / `stream_error` / `wrap_error`。

### 2.4 返回结果(dto.SynthesisResult)

```go
type SynthesisResult struct {
    AudioData  []byte        // 最终可直接回给客户端的音频字节(已收尾,如 wav 拼好头)
    Format     string        // 真实输出格式,决定 Content-Type
    SampleRate int
    ReqID      string        // 回写 X-Request-Id,排障用
    TextWords  int           // 上游计费字符数(取不到填 0)
    Chunks     int
    AudioBytes int
    TTFB       time.Duration // 首字节耗时
    Duration   time.Duration
}
```

`Format` 必须是主干认识的取值(见 `controller/tts.go:258` `contentTypeFor`):
`mp3` / `wav` / `pcm` / `ogg_opus`(别名 `opus`)。填别的会导致 Content-Type 错。

### 2.5 格式降级规则(容易踩)

客户端要的 `response_format` 和上游能给的格式**往往不一致**,必须显式降级:

| 客户端要 | 火山做法 | 为什么 |
|---|---|---|
| `wav` | 上游请求 `pcm`,本地 `WrapWAVHeader` 拼头 | 流式 wav 每 chunk 带独立头,拼接即坏 |
| `aac` / `flac` | **降级成 mp3**,Content-Type 报 `audio/mpeg` | 火山不支持 |
| `opus` | 上游 `ogg_opus` | 命名差异 |

新适配器同样要回答这个问题:**你要的格式上游给不了时,降级到什么?**
并且必须在 `Format` 字段里如实回报真实格式,否则客户端拿到的是"声明 mp3 实际 pcm"。

### 2.6 实测踩过的坑(上游协议层面)

这些是火山适配器真实修过的 bug,接新上游时同类问题大概率复现:

1. **事件分发必须白名单**:`ParseStream` 只把 `event == "sentence"` 或事件为空的帧
   当音频,未知事件只记日志**绝不拼进音频**(`response.go:71-113`)。
   早期实现把 `TTSSubtitle` 也当音频,导致音频流被污染。
2. **结束帧要提前 break**:上游用 `code == 20000000` 表示流正常结束,收到后要
   把剩余行读完再 break,否则连接不释放。
3. **单行可能很长**:`scanner.Buffer` 上限要放大(火山设到 8MB),否则大 chunk 会 EOF。
4. **所有音频帧 base64 解码失败要**返回错误,**不能跳过** —— 跳过等于静默产出半截音频。
5. **一个字节音频都没收到要报错**,不能返回空切片当成功(`response.go:125-131`)。
6. **错误响应体可能含换行**:直接拼进错误消息会伪造日志行,火山做了
   `\n`/`\r` 转义(`synthesis.go:99-101`),新适配器照做。

---

## 3. 目标架构:Provider 抽象(待实施)

以下代码**当前不存在**,是建议落地的设计。

### 3.1 接口定义

建议新增 `adapter/provider/provider.go`(新包,**不放在** `adapter/volcano` 里,
避免主干继续依赖具体实现):

```go
// Provider 是一个上游 TTS 服务的统一抽象。
// 实现者只需要关心:把我的能力说清楚、把一次合成做完。
type Provider interface {
    // Name 稳定标识,用于配置键、库表字段、metrics label。
    // 约定全小写无空格:volcano / openai / azure / aliyun / gpt-sovits。
    Name() string

    // Capabilities 声明本适配器支持什么。
    // 主干据此做校验与降级,不靠 if-else 猜实现。
    Capabilities() Capabilities

    // Synthesize 执行一次合成,阻塞到完成。
    // 必须遵守:ctx 取消即返回;上游不支持 clientFormat 时按 Capabilities 降级,
    // 并在 Result.Format 里如实回报真实格式。
    Synthesize(ctx context.Context, req Request, mtr MetricsRecorder) (*dto.SynthesisResult, error)
}

type Capabilities struct {
    // 上游原生能产出的格式;客户端要的格式不在其中时,主干负责降级或拒绝。
    Formats []string
    // 是否支持用倍率控制语速。不支持时主干忽略 speed 而不是报错。
    Speed bool
    // 是否按字符计费并回传用量(决定 UpstreamUsage 是否有意义)。
    Usage bool
    // 是否支持多音色路由。不支持时该 provider 只能配一个默认音色。
    Voices bool
    // 是否需要资源/部署/项目 ID 这类额外维度(火山需要,OpenAI 不需要)。
    ExtraScopes bool
}

// Request 是主干交给适配器的、已归一化的合成请求。
// 注意:这里刻意不出现任何厂商专属字段。
type Request struct {
    Text         string
    VoiceKey     string            // voices 表的主键语义(对外 voice 名或上游音色 ID)
    Model        string
    Format       string            // 客户端期望格式(主干已归一化:mp3/wav/pcm/ogg_opus)
    SampleRate   int
    Speed        float64           // 1.0 = 原速
    Language     string
    Extra        map[string]string // 厂商专属配置,从 voices/settings 的 JSON 列读

    // Credentials 由 provider 自己解释:
    // 火山读 api_key + resource_id;OpenAI 只读 api_key。
    Credentials Credentials
}

type Credentials struct {
    APIKey string
    // 厂商额外凭证/路由维度。火山: resource_id;Azure: region;自建: base_url。
    Scope map[string]string
}
```

### 3.2 注册表

```go
// adapter/provider/registry.go
func Register(p Provider)        // 通常在包的 init() 里调用
func Get(name string) (Provider, bool)
func Names() []string            // admin UI 下拉框用
```

**要点**:注册要放在**主干之外**。建议 `main.go` 显式空导入:

```go
import (
    _ "github.com/volcano-tts/tts-api/adapter/volcano"  // 注册 volcano
    _ "github.com/volcano-tts/tts-api/adapter/openai"   // 注册 openai
)
```

这样主干只依赖 `provider` 包,**新增上游不需要改主干任何一行** —— 这是本架构的核心收益,
请务必守住。`controller/tts.go` 里不应再出现 `adapter/volcano`。

---

## 4. 分阶段实施计划

**强烈建议按阶段做,每阶段结束都能编译、能跑、行为不变。**
一次性重构"配置 + 路由 + 库表 + 前端 + metrics"五个面,几乎必然做出半成品。

### 阶段 0 · 抽出接口,零行为变更(纯重构)

目标:主干不再依赖具体实现,但**行为完全不变**,回归风险最低。

- [ ] 新增 `adapter/provider` 包,放 `Provider` / `Capabilities` / `Request` / `Credentials` / `MetricsRecorder`
- [ ] 让 `volcano` 包实现 `Provider`(本质是把 `Options` 包一层适配,`Synthesis` 改成方法)
- [ ] `controller/tts.go`:把 `volcanoClient` + `volcano.Synthesis` 换成 `provider.Get(name).Synthesize`
- [ ] `MetricsRecorder` 从 `adapter/volcano` **移出**到 `provider` 包(否则接口还是火山专属)
- [ ] `setting.LoadRuntimeConfig` 暂时继续构造 `volcano.Options`,通过一个
      「volcano provider 的配置映射函数」转成 `provider.Request`
- [ ] 验收:`go build ./... && go vet ./... && go test ./... -count=1` 全绿,
      且 `/v1/audio/speech` 行为与重构前逐字节一致(拿同一段文本对比音频长度与格式)

> 这一阶段**不要**动库表、不要动前端。目标只有一个:主干里那行
> `import adapter/volcano` 消失。

### 阶段 1 · 数据结构:provider 维度入库

> ⚠️ **先纠正一个常见误解:本项目的迁移框架其实没有接线。**
> `store/migrate.go` 里定义了 `Migration` 结构体和 `migrations` 切片,但那个切片是
> **空的**,`store/db.go` 的 `migrate()` 也从**不**读取它 —— 注释里写得很明白:
> "本期 M0 阶段 schemaVersion=1,migrate() 在 db.go 内做基础建表,**未触发 migrations 调度**"。
> 所以 `migrate()` 目前**只做 `CREATE TABLE IF NOT EXISTS`**,没有任何版本比较,
> 对已存在的表**完全不做变更**。
>
> 还有一个既有瑕疵:`db.go:148-150` 的 `INSERT OR IGNORE INTO schema_version (version) VALUES (?)`
> 语句里既没有字面量也没传参,绑定到 `?` 的是**零值**,所以表里实际写入的版本号是 **0**,
> 不是常量 `schemaVersion`(=1)。做 v2 迁移时请一并修掉,否则版本判断从一开始就是错的。
>
> **结论**:给 `voices` 加列**不会**自动发生,你必须自己写 `ALTER TABLE`,
> 并且在启动时能被重复执行而不报错(老库、新库都要能跑)。

本阶段要做:

- [ ] 在 `migrate()`(或新启用的迁移调度)里实现**幂等**加列:
      先 `PRAGMA table_info(voices)` 查现有列,缺哪个 `ALTER TABLE ... ADD COLUMN` 哪个
      (SQLite 的 `ADD COLUMN` 不支持 `IF NOT EXISTS`,重复执行会报错,必须先查)
- [ ] `voices` 表加列(见 §5.1),老库回填 `provider='volcano'`
- [ ] 修正 `schema_version` 写入零值的瑕疵,并写出真实版本号
- [ ] 若确实要启用 `migrate.go` 的调度框架,则把 `schemaVersion`(db.go,`=1`)
      与 `schemaVersionRequested`(migrate.go,`=1`)一并提升到 2;
      注意 `installer/lock.go` 里的 `schemaVersion = "1"`(字符串)**是另一套东西**——
      它写在 `installed.lock` 里做安装标记,与库表结构版本是两回事,
      两者要一起提升就一起提升,不要只改一边造成语义分裂
- [ ] `store.Voice` 结构体加字段 + JSON tag
- [ ] `store.GetVoiceForTTS` 返回值扩展(见 §5.2),`setting.Store` 接口同步改
- [ ] `cmd/dumpdb` 输出新列
- [ ] 验收:**用一个装了老数据的 `tts.db`** 启动服务(不要用全新库测),
      确认自动加列成功、数据不丢、`/v1/audio/speech` 仍可用;
      再重启一次确认迁移幂等(第二次不报错)

### 阶段 2 · 配置:按 provider 命名空间化

现状是 `settings` 表用**扁平键**,且键名全是火山语义(`default_resource_id`、
`default_speaker`、`model_type`…)。多上游后必须命名空间化。

- [ ] 保留全局键:`auth_key` / `cors_*` / `timeout` / `default_provider`
- [ ] 厂商专属键改为 `provider.<name>.<key>`,例如
      `provider.volcano.api_key`、`provider.volcano.resource_id`、
      `provider.openai.api_key`、`provider.openai.base_url`
- [ ] `LoadRuntimeConfig` 只装配**当前 `default_provider`** 的 `Request`,
      不再构造 `volcano.Options`
- [ ] `setting.CheckEnvironmentVariables` / `LogStartupSummary` 去掉
      `BYTEDANCE_TTS_*` 硬编码名,改成遍历 provider 的必填项(否则 `/health` 会误导运维)
- [ ] 迁移:老键 `api_key` → `provider.volcano.api_key` 等,一次性搬完并保留旧键只读兜底一轮
- [ ] 验收:除火山外没有任何 provider 时,服务行为仍与阶段 1 相同

### 阶段 3 · 观测:metrics 加 provider 维度

- [ ] `tts_upstream_total` / `tts_upstream_duration_seconds` / `tts_upstream_errors_total`
      等**全部上游指标加 `provider` label**
- [ ] `metrics.AdapterRecorder` 增加 provider 字段(构造时注入 name)
- [ ] 仪表盘 PromQL 补 `by (provider)`
- [ ] ⚠️ **破坏性变更**:加 label 会让现有告警规则里的
      `tts_upstream_total{...}` 聚合口径变化,发布说明里要写明
- [ ] 验收:`/metrics` 上能按 provider 拆分成功率与 P95

### 阶段 4 · 管理界面:provider 选择与音色归属

- [ ] `/setup` 向导第 1 步加"上游类型"选择,后续步骤按所选 provider 动态渲染字段
      (火山显示 resource_id,**OpenAI 显示 base_url 且不显示 resource_id**)
- [ ] `router/admin-voices.html` 新增/编辑音色时选 provider 与 model
- [ ] `/api/voices` 的请求/响应加 `provider` 字段,`controller/admin.go` 校验
      "provider 必须已注册",否则 400(不要留到合成时才 500)
- [ ] 删除 provider 最后一个音色时的保护逻辑(现在的 `ErrInUse` 只管 `default_speaker`)
- [ ] 验收:UI 上能并存火山与 OpenAI 音色,各自路由正确

### 阶段 5 · 第二个真实适配器(OpenAI 兼容)

**这一步才是对抽象的真正验收**。选 OpenAI 兼容上游(或其代理)最省事,因为
本项目对外就是 OpenAI 形状 —— 可以直接验证"请求/响应穿透"是否正确。

- [ ] 新建 `adapter/openai/`,只实现 §3.1 的接口
- [ ] 全程**不改** `controller/` `setting/` `store/` 任何一行
- [ ] 如果为了实现它而必须改主干,说明 §3.1 的抽象漏了维度 —— **回头修接口,而不是打补丁**
- [ ] 验收:同一份 `voices` 配置里两个 provider 可同时服务,
      客户端只看到 `voice` 名不同

---

## 5. 数据模型改动细节

### 5.1 voices 表

现状(真实 schema,`store/db.go:129-142`):

```sql
CREATE TABLE IF NOT EXISTS voices (
    id          INTEGER PRIMARY KEY AUTOINCREMENT,
    name        TEXT NOT NULL UNIQUE,
    speaker     TEXT NOT NULL,      -- 上游音色 ID
    resource_id TEXT NOT NULL,      -- ← 火山专属概念,却写成 NOT NULL 通用列
    model       TEXT DEFAULT '',
    language    TEXT DEFAULT '',
    description TEXT DEFAULT '',
    enabled     INTEGER NOT NULL DEFAULT 1,
    created_at  TEXT NOT NULL DEFAULT (datetime('now')),
    updated_at  TEXT NOT NULL DEFAULT (datetime('now'))
);
```

**问题**:`resource_id` 是火山独有概念(决定模型版本与计费),却成了全表必填列。
OpenAI 上游根本没有这个字段 —— 这正是"单上游假设"渗进数据模型的典型症状。

建议改为:

```sql
ALTER TABLE voices ADD COLUMN provider TEXT NOT NULL DEFAULT 'volcano';
ALTER TABLE voices ADD COLUMN vendor_params TEXT NOT NULL DEFAULT '{}';  -- JSON,厂商私有
-- resource_id 保留但放宽为可空(或整体搬进 vendor_params,看迁移成本)
```

- `provider` 建索引:`CREATE INDEX idx_voices_provider ON voices(provider)`
- `vendor_params` 存厂商私有键值(火山:`{"resource_id":"seed-icl-2.0","model_type":4}`),
  主干**不解释**它的内容,原样交给对应 provider
- 迁移:`UPDATE voices SET provider='volcano' WHERE provider IS NULL OR provider=''`

### 5.2 GetVoiceForTTS 的签名

现状(`setting/config.go:426-436`,刻意用 5 个返回值避免循环 import):

```go
GetVoiceForTTS(name string) (speaker, resourceID, model string, found bool, err error)
```

多上游后要扩成:

```go
// 返回该 voice 的 provider 名 + 厂商私有参数,主干据此把请求路由给对应适配器。
GetVoiceForTTS(name string) (providerName, speakerID, model string, vendorParams map[string]string, found bool, err error)
```

**注意**:当年之所以不用 `(VoiceRef, error)`,是为了让 `store` 不 import `setting`
(避免循环 import)。加字段时**别再引入新的结构体依赖**,继续用扁平返回值或
定义在 `store` 包里的中性结构体。

---

## 6. 新增一个适配器的完整清单

假设要加 `foo` 上游:

**代码**

1. `adapter/foo/foo.go` —— 实现 `provider.Provider`(`Name()` 返回 `"foo"`)
2. `adapter/foo/request.go` —— `provider.Request` → foo 的 HTTP 请求体
3. `adapter/foo/response.go` —— 解析 foo 的响应;严格遵守 §2.6 的 6 条协议纪律
4. `adapter/foo/errors.go` —— 错误要带阶段,`Stage` 用统一枚举
5. `adapter/foo/foo_test.go` —— 至少覆盖:正常流、未知事件不污染音频、空音频报错、
   base64 解码失败报错、格式降级回报正确 `Format`、`ctx` 取消能及时返回
   (测试文件按项目策略不入库,本地保留并在合并前手动跑)
6. 在 `foo.go` 的 `init()` 里 `provider.Register(...)`
7. `main.go` 加一行空导入

**配置**

8. 定义必填项清单(供 `/health` 与启动摘要显示),不要写死在前端
9. 在 `/setup` 向导里加该 provider 的字段定义(阶段 4 之后是数据驱动,只需加配置)

**验收**

10. `go build ./... && go vet ./... && go test ./... -count=1` 全绿
11. 起服 → `/setup` 选 foo → 填凭证 → 加一个音色 → `curl /v1/audio/speech` 出声
12. 故意填错凭证 → 客户端应拿到明确的 4xx/5xx 与 `code`,日志里凭证是**打码**的
13. 不支持的 `response_format` → 按 `Capabilities` 降级,且 Content-Type 与真实字节一致

---

## 7. 测试策略(注意本项目的特殊约束)

**本项目测试文件不入库**(`.gitignore` 里的 `*_test.go`),
测试保留在本地磁盘。这带来两个直接后果,必须知道:

1. **CI 跑不到你的测试**。`.github/workflows/ci.yml` 里的 `go test` 步骤在干净克隆上
   只会打印 `no test files` 然后通过 —— 它只能守住"编译 + vet"。
   所以适配器测试**必须在本地跑过再合并**:
   ```bash
   go test ./... -count=1
   ```
2. **不要依赖测试文件来传递知识**。协议细节、踩坑记录要写进**代码注释**和本文档,
   否则对下一个人(以及在 CI 里)等于不存在。

适配器测试的**最小可用集合**(按 §2.6 的坑逐条覆盖):

| 用例 | 断言 |
|---|---|
| 正常流 | 音频字节非空;`Chunks` / `TTFB` / `Format` 合理 |
| 未知事件帧 | 不进入音频;不报错;不改变 `Chunks` |
| 字幕帧 | 进 `Subtitles`,**不进** `AudioData` |
| 全流无音频 | 返回错误,不是"成功 + 空字节" |
| base64 损坏 | 返回错误,不是跳过继续 |
| 超长单行(>1MB) | 不 EOF |
| 上游 HTTP 4xx/5xx | 错误 `Stage == "http"`,携带 code |
| `ctx` 取消 | 及时返回,不泄漏 goroutine |
| 格式降级 | `Result.Format` 是真实格式,不是客户端要的格式 |

---

## 8. 反模式清单(明确不要做)

- ❌ **不要在 `controller/` 里写 `switch providerName` 分发**。
  分发只能走注册表,否则每加一个上游都要改主干,抽象就白做了。
- ❌ **不要让 `provider` 包 import 任何具体适配器**。依赖方向永远是
  `具体适配器 → provider 包 ← 主干`。
- ❌ **不要在 `provider.Request` 里加厂商专属字段**(如 `ResourceID`)。
  厂商私有参数一律走 `Credentials.Scope` / `Request.Extra` / `voices.vendor_params`。
- ❌ **不要把上游返回的原始音频直接当结果返回而不收尾**。
  PCM 要拼头,降级要如实回报 `Format`(见 §2.5)。
- ❌ **不要静默跳过解析失败的帧**(§2.6 第 4 条)。
- ❌ **不要在日志里打印明文凭证或音色 ID**。沿用
  `telemetry.MaskSpeaker` / `MaskResourceID`,新适配器的私有敏感字段也要加等价打码。
- ❌ **不要用一个 `settings` 键名承载多个 provider 的配置**。
- ❌ **不要为了省事把 provider 名写进 `settings` 的扁平键里当"事实上的路由"**,
  路由的唯一权威是 `voices.provider` 与 `default_provider`。

---

## 9. 关键文件索引

| 关注点 | 文件 | 说明 |
|---|---|---|
| 请求入口 / 校验 / 路由 | `controller/tts.go` | `resolveClientFormat`、voice 查库、错误归一 |
| 合成编排参考实现 | `adapter/volcano/synthesis.go` | 埋点顺序、格式降级、错误分阶段 |
| 流解析参考实现 | `adapter/volcano/response.go` | 事件白名单、结束帧、base64 |
| 传输层参考实现 | `adapter/volcano/client.go` | 连接复用、nil 安全 |
| 配置装配 | `setting/config.go` | `LoadRuntimeConfig`、`setting.Store` 接口 |
| 库表与迁移 | `store/db.go`、`store/migrate.go` | `schemaVersion`、`migrate()`(**迁移框架未接线,见阶段 1 警示**);`installer/lock.go` 的 `schemaVersion` 是另一套 |
| 音色模型 | `store/voices.go` | `Voice`、`GetVoiceForTTS` |
| 指标 | `metrics/metrics.go` | `AdapterRecorder`、`codeLabel` |
| 结果类型 | `dto/tts.go` | `SynthesisResult`、`SubtitleEntry` |
| 未来抽象落点 | `adapter/provider/` | **尚不存在,待阶段 0 创建** |

---

*文档版本:v1 · 对应代码基线:`develop` @ `ad342ec`(2026-10-03)*
*本文「现状」部分与代码逐行核对过;「目标架构」为提案,实施时请同步更新本文档。*
