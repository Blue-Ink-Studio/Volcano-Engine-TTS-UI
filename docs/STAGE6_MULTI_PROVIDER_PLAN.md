# STAGE6 · 多服务商聚合 —— 实施契约（冻结版 v1.5）

> 基线：`develop @ 7da40d6`（STAGE1–5 已落地）
> 上游输入：《STAGE6-MULTI-PROVIDER-AGGREGATION》v1.2 → 契约 v1.3 → v1.4 → **本版**
> 本版在 v1.4 基础上定案 **9 条细节（G1–G9）**，全部由实现方决定（策划已放权）。
> 前置事实：**无生产环境** ⇒ 迁移可清空音色、删兼容分支、不做过渡。

---

## §1 目标与范围

### 1.1 目标

「**渠道即模型（Channel-as-Model）**」：`channel.name` 就是 OpenAI 侧 `model` 名，渠道内音色就是 `voice`。

```http
POST /v1/audio/speech
{ "model": "openai-main", "voice": "alloy", "input": "你好" }
        │                    │
        │                    └─→ voices WHERE channel_id=<渠道> AND name='alloy'
        └─→ channels WHERE name='openai-main' → provider=openai
```

- 新增 **OpenAI** adapter（`tts-1` / `tts-1-hd`）
- 新增 **Azure Speech** adapter（SSML）
- 音色归属渠道、渠道级默认音色、`provider.Request` 去火山化

### 1.2 明确不做（Follow-up 梯队）

组隔离启用（`user_group`/`channel_group` 匹配）、流式 SSE、多渠道 failover、per-channel 计费倍率、缓存/分片/字幕/ASR。

### 1.3 与 v1.2 的关键差异

| 事项 | v1.2 说法 | 本版 |
|---|---|---|
| 老库兼容 | "不新增 channel 时行为完全一致（含不带 model）" | **删掉**：model 硬性，不带 → 400 `model_required` |
| 音色唯一性 | 迁就 `UNIQUE(name)` | **重建 voices 表**（表级 UNIQUE 无法 DROP，见 §4.1） |
| failover | 未表态 | **本期不用**：model 精确命中唯一渠道 |

---

## §2 决策总表

### 2.1 决策层（v1.3 的 12 条，零修改）

| 编号 | 决策 |
|---|---|
| D1 | 渠道即模型：`channel.name` = 对外 model，音色归属渠道 |
| A1 | **model 硬性**，不带 → 400 `model_required`，不做兼容 |
| A2 | **渠道级 `default_voice`**，废弃全局 `settings.default_speaker` |
| A3 | **`voices.channel_id` 权威**，`channels.voices` CSV 保留但不读 |
| A4 | 三个 model 概念分离；`voices.resource_id` 允许空串 |
| B5 | model 命中**唯一**渠道，绕过 failover；`priority/weight/auto_ban` 暂不生效 |
| B6 | `usage.model` 落 **对外 model**（= `channel.name`） |
| B7 | 删火山零渠道兜底死代码 |
| B8 | `provider.Request` 去火山化 |
| B9 | 各 adapter 自行降级并回写 `Result.Format` |
| C10 | OpenAI + Azure 都做；先 OpenAI 全链路，Azure 次之 |
| C11 | `GET /api/admin/providers` |
| C12 | 迁移**清空重建**音色（无生产环境） |

### 2.2 执行层细节定案（本版新增，G1–G9）

| 编号 | 定案 | 理由 |
|---|---|---|
| **G1** | **基表 DDL 同步改成新结构**（`db.go` 的 `CREATE TABLE voices` 去掉表级 `UNIQUE`、加 `channel_id`）；迁移块**只服务老库** | 若基表留旧结构，全新库一旦迁移 WARN 失败（契约允许不阻断），会**静默停留在全局唯一**、且无报错。改基表后迁移失败对全新库无害 |
| **G2** | **删除 `store.SetupApply` + `voiceInsertTx`** | STAGE4 遗留、STAGE5 起已无调用方；它们插入不带 `channel_id` 的音色，新架构下是**永远路由不到的孤儿行** |
| **G3** | **C11 去掉"门户复用"用途**，`/api/admin/providers` 只服务管理端；门户的 model 列表**不在本期** | `/api/admin/providers` 挂 `RequireAdminOrJWT`，门户是普通用户 JWT → 必 401。且 `group` 隔离未做，现在把渠道名暴露给普通用户不合适 |
| **G4** | **`default_voice` 由音色改名/删除同步维护**：改名 → 同事务把所有指向旧名的 `channels.default_voice` 改成新名；删除 → 被引用时**自动清空为 `''` + WARN**，不阻断删除 | 删 `default_speaker` 时连带删掉了那段同步逻辑，`channels.default_voice` 需要等价的一份。选择"清理"而非"拒绝"是为了与"删掉 `ErrInUse` 保护"保持一致（不阻断管理操作） |
| **G5** | **`VoiceInsert` / `VoiceUpdate` 强制 `channel_id`**：`channel_id <= 0` 或渠道不存在 → 400 `invalid_channel` | §3.2 说"NULL 仅历史兜底行"；不写死就会重新长出孤儿音色 |
| **G6** | **`voices.model` 为空时的 adapter 默认**：openai → `tts-1`；volcano → 回落 `settings.model`；azure → 不使用 | 三个 adapter 各写各的默认值会不一致；写进契约统一 |
| **G7** | **`unknown_model` 同一个 code，message 区分**：不存在 → `unknown model: 'x'`；命中但禁用 → `model 'x' is disabled`。`model_required` 的 message **列出可用 model** | 同 code 便于前端分支，message 区分便于排障 |
| **G8** | **provider 注册校验只在 `controller/channels.go`**；`controller/admin.go` 只做 voice CRUD 按 id + `/api/admin/providers` | v1.4 清单同一件事列了两个文件 |
| **G9** | **删除 `dto.HealthResponse.ConfigStatus` 整个字段**（连同 `ConfigStatusResponse` 类型） | 它只承载 `GetTTSConfigErr()`，而后者本期整个删除。已确认前端 11 个页面**零引用** `config_status` / `config_error`，直接删安全 |

### 2.3 补充定案（实现方一并决定）

| 事项 | 定案 |
|---|---|
| 迁移方式 | **`DROP TABLE` + `CREATE TABLE`**（C12 反正清空，省一次 rename） |
| 旧前端仍传 `default_speaker` | **静默忽略 + WARN**，不返回 400（无害字段，别让旧管理页报错） |
| `channels.name` 校验 | `^[a-zA-Z0-9_-]{1,64}$`（与 `voices` 的 `voiceNameRe` 一致）；违规 → 400 `invalid_channel` |
| WAV 头工具 | 下沉到 `common`：`common.WrapWAVHeader` / `common.StripWAVHeader`；`adapter/volcano.WrapWAVHeader` 保留为**薄封装**（避免改既有测试） |
| OpenAI 采样率 | 忽略 `Request.SampleRate`，**如实回报 24000**（OpenAI TTS 固定 24kHz） |
| OpenAI 不支持的格式 | `aac` / `flac` → 降级 **mp3**，回写 `Result.Format="mp3"` |
| Azure 采样率 | 忽略 `Request.SampleRate`，**如实回报 24000** |
| `voice_not_found` | **废弃**（B5 后不再产生）；voice 错误只剩 `voice_required` / `unknown_voice` |
| `missing_provider` | **退役**（并入 `invalid_provider`） |
| `provider_unavailable` | **保留**（运行时渠道的 provider 未注册 → 503） |

---

## §3 Schema（精确）

### 3.1 `channels`

```sql
-- 新增列（ensureColumn 幂等，放事务外）
default_voice TEXT NOT NULL DEFAULT ''
```

- `priority` / `weight` / `auto_ban` **保留但不生效**（B5）
- `name` 增加正则校验 `^[a-zA-Z0-9_-]{1,64}$`（它现在是对外 model 名）
- `voices`（CSV 白名单）**保留列但不读**（A3）

### 3.2 `voices`（重建后）

```sql
CREATE TABLE voices (
    id          INTEGER PRIMARY KEY AUTOINCREMENT,
    channel_id  INTEGER,                      -- 归属渠道；NULL 仅历史兜底行
    name        TEXT NOT NULL,                -- 不再 UNIQUE（渠道内唯一靠复合索引）
    speaker     TEXT NOT NULL,                -- 上游音色标识
    resource_id TEXT NOT NULL DEFAULT '',     -- 非火山渠道写空串
    model       TEXT DEFAULT '',              -- 上游 model 标识
    language    TEXT DEFAULT '',
    description TEXT DEFAULT '',
    enabled     INTEGER NOT NULL DEFAULT 1,
    created_at  TEXT NOT NULL DEFAULT (datetime('now')),
    updated_at  TEXT NOT NULL DEFAULT (datetime('now'))
);

CREATE UNIQUE INDEX idx_voices_channel_name ON voices(channel_id, name);
```

- **关键**：`name` 上**没有表级 `UNIQUE`**。旧的 `name TEXT NOT NULL UNIQUE` 生成了 `sqlite_autoindex_voices_1`，`DROP INDEX` 删不掉它（见 §4.1）
- `NULL channel_id` 行不受复合唯一约束（SQLite 中 NULL 互不相等）

### 3.3 `settings`

- `default_speaker` 的 key **保留但停用**（便于回滚）
- `GET/PATCH /api/admin/settings` 收到 `default_speaker` → 忽略 + WARN（§2.3）

---

## §4 迁移（`schema_version` 6 → 7）

### 4.1 为什么必须重建表（实测结论）

`store/db.go:161` 是 **`name TEXT NOT NULL UNIQUE` —— 表级约束**，SQLite 会自动建 `sqlite_autoindex_voices_1`。`store/db.go:173` 那个 `idx_voices_name` 只是**冗余的第二个**索引。

实测（`modernc.org/sqlite`，与仓库同一 driver）：

```
STEP2 DROP INDEX OK
STEP4 插入第二条（同名/不同渠道）被拒
      => UNIQUE constraint failed: voices.name (2067)
      ⇒ 表级 UNIQUE 仍在，DROP INDEX 无效
```

SQLite 没有 `ALTER TABLE ... DROP CONSTRAINT`。唯一正解是重建表。

### 4.2 迁移实现

```go
const stage6SchemaVersion = 7

// applyStage6Migration 是 6→7 的一次性迁移，单事务完成四件事：
//  1. 按新结构重建 voices（去表级 UNIQUE(name)，引入 channel_id）
//  2. C12：清空音色（不搬数据）
//  3. 建 (channel_id, name) 复合唯一索引
//  4. 版本记账 —— 必须与 1~3 同事务（STAGE5 §4.2 的教训）
//
// 失败 → 回滚 + WARN 不阻断启动，下次启动重跑。
func (s *Store) applyStage6Migration() error {
	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("store: stage6 begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	if _, err := tx.Exec(`CREATE TABLE voices_new (...)`); err != nil { ... }
	if _, err := tx.Exec(`DROP TABLE voices`); err != nil { ... }
	if _, err := tx.Exec(`ALTER TABLE voices_new RENAME TO voices`); err != nil { ... }
	if _, err := tx.Exec(`CREATE UNIQUE INDEX IF NOT EXISTS idx_voices_channel_name
	                      ON voices(channel_id, name)`); err != nil { ... }
	if _, err := tx.Exec(`INSERT OR IGNORE INTO schema_version (version) VALUES (?)`,
		stage6SchemaVersion); err != nil { ... }
	return tx.Commit()
}
```

门控（在 `migrate()` 里，位置对齐 STAGE5）：

```go
curVersion := s.currentSchemaVersion()          // migrate() 开头已读
// ensureColumn(channels, "default_voice", "TEXT NOT NULL DEFAULT ''")  ← 事务外
if curVersion < stage6SchemaVersion {
    if err := s.applyStage6Migration(); err != nil {
        log.Printf("[store] WARN: STAGE6 迁移失败，不阻断启动（下次重跑）: %v", err)
    }
}
```

- `channels.default_voice` 的 `ensureColumn` 放**事务外**（它自带 `PRAGMA table_info`，单连接下事务内容易自锁）
- voices 重建放**事务内**（实测 DDL 可进事务）
- 版本记账用 `schema_version` **行表**：`INSERT OR IGNORE INTO schema_version (version) VALUES (7)`
  （**不是** `schema_meta`，那张表不存在；实测会报 `no such table: schema_meta`）

### 4.3 基表 DDL 同步改（G1）

`store/db.go` 的 `CREATE TABLE IF NOT EXISTS voices` **也改成 §3.2 的新结构**。

- 全新库：直接得到正确结构；即便迁移 WARN 失败也无害
- 老库：`IF NOT EXISTS` 不覆盖，由迁移块重建

### 4.4 实测确认

```
rebuild committed OK
same name in channel 1: <nil>
same name in channel 2: <nil>
same name twice in SAME channel (should fail): UNIQUE constraint failed: voices.channel_id, voices.name
schema_version after rebuild = 7
```

---

## §5 请求路由与错误码

### 5.1 判定顺序（写死）

```
方法校验 → installer 模式(503 install_required)
→ 鉴权(401/403)
→ db_not_ready(503)
→ no_channel(503)                    ← 保持读 body 之前
→ [读 body + JSON 解析 + 基础校验]
→ model_required(400) → unknown_model(400)
→ voice_required(400) → unknown_voice(400)
→ 预扣(402)
→ 合成
```

**`no_channel` 优先于 `model_required`**：连渠道都没有时先报"没有渠道"，加完渠道才报"要带 model"。

### 5.2 错误码表

| code | HTTP | 触发 | 变化 |
|---|---|---|---|
| `no_channel` | 503 | 无 enabled 渠道 | 沿用（STAGE5） |
| `model_required` | 400 | 请求未带 `model`（message 列出可用 model） | 新增 |
| `unknown_model` | 400 | model 未命中渠道 / 命中但禁用（message 区分） | 新增 |
| `voice_required` | 400 | 无 `voice` 且渠道 `default_voice` 为空 | 新增 |
| `unknown_voice` | 400 | 渠道内无该 voice | 沿用 |
| ~~`voice_not_found`~~ | — | 无渠道支持该 voice | **废弃** |
| `invalid_channel` | 400 | `channel_id` 不存在 / 非法；`channel.name` 字符不合法 | 新增 |
| `invalid_provider` | 400 | provider 为空或未注册 | 新增（并入 `missing_provider`） |
| ~~`missing_provider`~~ | — | provider 为空 | **退役** |
| `provider_unavailable` | 503 | 运行时渠道 provider 未注册 | 沿用 |
| `voice_duplicate` | 409 | `(channel_id, name)` 撞唯一 | 新增 |
| `db_not_ready` | 503 | store 未就绪 | 沿用 |

> 前端**必须按 `code` 分支**，不要按 HTTP 状态码：`no_channel` / `provider_unavailable` / `db_not_ready` **同为 503**。

### 5.3 路由实现要点

1. `model` → `store.ChannelGetByName(model)`（唯一命中，`channels.name` 有 UNIQUE）
2. 未命中 → `unknown_model`；命中但 `status != 1` → `unknown_model`（message: disabled）
3. `voice` 为空 → 用该渠道 `default_voice`；仍为空 → `voice_required`
4. `store.VoiceResolve(channelID, voiceName)`；未命中 → `unknown_voice`；`enabled=0` → 403 `voice_disabled`（沿用）
5. 直接调 `provider.Get(channel.Provider).Synthesize(...)`，**不经** `route.Router.Select`

---

## §6 三个 model 的流向（写死）

| 概念 | 来源 | 去向 |
|---|---|---|
| **对外 model** | `channel.Name` | `usage.model`、metrics label、日志、响应 |
| **上游 model** | `voices.Model`（空则按 §6.1 回落） | `provider.Request.Model` → adapter 发给上游 |
| 火山复刻 model | `settings.model` | 仅火山 adapter 内部 |

### 6.1 `voices.model` 为空时

| provider | 回落 |
|---|---|
| openai | `tts-1` |
| volcano | `settings.model`（现有 `setting/config.go:396-399` 逻辑） |
| azure | 不使用（SSML 只有 `<voice name>`） |

### 6.2 必改两处（否则静默落错）

- `controller/tts.go` 的 `settleUsage`：`usage.Model = channel.Name`（**不能**用 `ttsReq.Model`，那是上游 model）
- `controller/tts.go` 的退款 defer：`Refund(store.Usage{Model: channel.Name})` 同步
- 构造 `provider.Request` 时：`Model = voiceRow.Model`（上游 model）

---

## §7 adapter 边界

### 7.1 OpenAI（`adapter/openai`）

- 端点：`{base_url}/v1/audio/speech`，`base_url` 默认 `https://api.openai.com`，可由 `Credentials.Scope["base_url"]` 覆盖（兼容第三方 OpenAI 兼容上游）
- 鉴权：`Authorization: Bearer <api_key>`（`Scope` 其余键忽略）
- 请求体：`{model, input, voice, response_format, speed}`
- 格式映射：

| 对外格式 | 上游 `response_format` | `Result.Format` |
|---|---|---|
| `mp3` | `mp3` | `mp3` |
| `wav` | `wav` | `wav` |
| `pcm` | `pcm` | `pcm` |
| `ogg_opus` | `opus` | **`ogg_opus`**（OpenAI 的 opus 就是 OGG/Opus 容器） |
| `aac` / `flac` | `mp3`（降级） | `mp3` |

- 采样率：忽略 `Request.SampleRate`，`Result.SampleRate = 24000`
- `speed`：0.25–4.0（与 `common.MinSpeed/MaxSpeed` 一致，无需特殊处理）
- 上游 401/403 → `UpstreamError{IsAuth()==true}`，不 500、不泄漏 key

### 7.2 Azure（`adapter/azure`）

- 端点：`https://{region}.tts.speech.microsoft.com/cognitiveservices/v1`
- 鉴权：`Ocp-Apim-Subscription-Key: <api_key>`
- 请求体：**SSML**
  ```xml
  <speak version='1.0' xml:lang='zh-CN'>
    <voice name='zh-CN-XiaoxiaoNeural'>
      <prosody rate='+0%'>文本</prosody>
    </voice>
  </speak>
  ```
  - `voice name` ← `Request.VoiceKey`
  - `rate` ← 由 `Request.Speed` 换算成百分比（`(speed-1)*100`，格式 `+N%` / `-N%`）
- 输出格式（`X-Microsoft-OutputFormat`）：

| 对外格式 | 上游格式 | 后处理 | `Result.Format` |
|---|---|---|---|
| `mp3` | `audio-24khz-48kbitrate-mono-mp3` | — | `mp3` |
| `wav` | `riff-24khz-16bit-mono-pcm` | 已含 RIFF 头，直接返回 | `wav` |
| `pcm` | `riff-24khz-16bit-mono-pcm` | **剥掉 44 字节 RIFF 头** | `pcm` |
| `ogg_opus` / `aac` / `flac` | `audio-24khz-48kbitrate-mono-mp3`（降级） | — | `mp3` |

- 采样率：固定 24kHz，`Result.SampleRate = 24000`（忽略 `Request.SampleRate`）
- `Scope`：`region`（必填）、`base_url`（可选覆盖）

### 7.3 WAV 头工具下沉（§2.3）

- `common.WrapWAVHeader(pcm []byte, sampleRate int) ([]byte, error)`
- `common.StripWAVHeader(wav []byte) ([]byte, error)` —— 校验 `RIFF`/`WAVE` 标识
- `adapter/volcano.WrapWAVHeader` 保留为薄封装（**避免改动既有测试**）

---

## §8 对外契约变更

### 8.1 voice CRUD 改按 id 寻址（M3）

**原因**：同名跨渠道合法后，`{name}` 无法定位。

| 方法 | 旧 | 新 |
|---|---|---|
| DELETE | `/api/admin/voices/{name}` | `/api/admin/voices/{id:[0-9]+}` |
| PATCH | `/api/admin/voices/{name}/toggle` | `/api/admin/voices/{id:[0-9]+}/toggle` |
| DELETE | `/api/voices/{name}` | `/api/voices/{id:[0-9]+}` |
| PATCH | `/api/voices/{name}/toggle` | `/api/voices/{id:[0-9]+}/toggle` |

- `GET /api/voices`（列表）与 `POST /api/voices`（创建）路径不变
- **创建/更新的 body 增加 `channel_id`（必填）**
- 列表回显 `channel_id` + `channel_name`

### 8.2 新增 `GET /api/admin/providers`

```json
{ "providers": ["azure", "openai", "volcano"] }
```

`RequireAdminOrJWT`；数据源 `provider.Names()`。**仅服务管理端**（G3）。

### 8.3 `GET /api/admin/channels` 增加 `default_voice`

渠道视图与创建/更新 body 均增加 `default_voice`。

---

## §9 清理清单（删死代码 / 删误导）

| 删除对象 | 原因 |
|---|---|
| `store.SetupApply` + `voiceInsertTx` | STAGE4 遗留、无调用方、会造孤儿音色（G2） |
| `setting.GetTTSConfigErr` + `SetTTSConfigErr` + 全库必填项判定 | 渠道化后不存在"全局必填配置"（B7） |
| `setting.GetTTSRequest` + `ttsOptions volcano.Options` 全局模板 | 渠道无关化（B8） |
| `setting.GetDefaultProviderName` | 零渠道兜底已删（B7） |
| `controller/tts.go` 的 `rtr.Empty()` 零渠道兜底分支 | STAGE5 起已 503 不可达（B7） |
| `dto.HealthResponse.ConfigStatus` + `ConfigStatusResponse` | 只承载 `GetTTSConfigErr`（G9） |
| `store/voices.go` 的 `default_speaker` 同步与 `ErrInUse` 保护 | A2 废弃全局默认音色 |
| `controller/admin.go` 的 `voice_in_use` 分支 | 同上 |
| `route.AcceptsVoice` 参与音色判定（函数可留，主路径不调用） | A3 |
| `voice_not_found` 错误码 | B5 后不再产生 |

---

## §10 代码改动清单

| 位置 | 改动 |
|---|---|
| `store/db.go` | ① 基表 DDL 改新结构（G1）② `channels.default_voice` ensureColumn ③ `applyStage6Migration` + 门控 ④ `schemaVersion = 7` |
| `store/voices.go` | `Voice.ChannelID`；`VoiceResolve(channelID,name)`；`channel_id` 强制校验（G5）；删 `default_speaker` 同步/`ErrInUse`；改名/删除时同步 `channels.default_voice`（G4）；`resource_id` 允许空串 |
| `store/channels.go` | `Channel.DefaultVoice`；`ChannelGetByName`；`name` 正则校验；`default_voice` 读写 + 指向校验 |
| `store/setup.go` | 删 `SetupApply` + `voiceInsertTx`（G2） |
| `common/wav.go`（新） | `WrapWAVHeader` / `StripWAVHeader` |
| `adapter/volcano/audio.go` | 改为调用 `common.WrapWAVHeader`（薄封装） |
| `adapter/volcano/provider.go` | `BuildRequest`/`optionsFromRequest` 收敛，不再依赖全局 `ttsOptions` |
| `adapter/provider/provider.go` | `Request` 去火山化；`Credentials.Scope` 语义文档化 |
| `adapter/openai/provider.go`（新） | OpenAIProvider（§7.1） |
| `adapter/azure/provider.go`（新） | AzureProvider（§7.2） |
| `setting/config.go` | 删 `GetTTSConfigErr`/必填判定/`GetTTSRequest`/`GetDefaultProviderName`；`LogStartupSummary` 去必填项打印 |
| `controller/tts.go` | model→channel→voice 路由；判定顺序；错误码；`usage.Model=channel.Name`；删零渠道兜底；`/health` 只判 `db_not_ready` |
| `controller/admin.go` | voice CRUD 按 id；删 `voice_in_use`；`GET /api/admin/providers` |
| `controller/channels.go` | 建/改校验 provider 已注册（`invalid_provider`）；`default_voice` |
| `controller/settings.go` | `default_speaker` 静默忽略 + WARN |
| `router/router.go` | voice 路由改 `{id}`；注册 `/api/admin/providers` |
| `dto/health.go` | 删 `ConfigStatus` / `ConfigStatusResponse`（G9） |
| `main.go` | 空导入 `adapter/openai` / `adapter/azure` |
| `docs/UPSTREAM_ADAPTER_GUIDE.md` | 补 OpenAI / Azure 完整范例 |
| `router/admin-channels.html` | 渠道内管音色；provider 下拉动态；`priority/weight` 标"暂不生效"；`default_voice` |
| `router/admin-voices.html` | 按 id 操作；渠道归属；重建提示 |

---

## §11 实施顺序

```
1. store 迁移先行（基表 DDL + channels.default_voice + voices 重建 + v7）—— 地基
2. store CRUD（ChannelGetByName / VoiceResolve / channel_id 强制 / G4 同步）
3. common/wav + provider.Request 去火山化 + setting 清理
4. adapter/openai（全链路）
5. adapter/azure
6. controller/tts.go 路由改造 + 错误码 + usage.Model
7. controller/admin.go（voice by id + providers）+ channels.go（provider 校验）
8. /health 修复 + dto 清理
9. 前端（用户负责）
10. 验收 §12
```

---

## §12 验收清单

1. 不带 `model` → 400 `model_required`（message 列出可用 model）
2. `model` 不存在 → 400 `unknown_model`；命中但禁用 → 400 `unknown_model`（message 含 disabled）
3. 零渠道 + 不带 model → **503 `no_channel`**（优先于 400）
4. 火山渠道 `huoshan` + 音色 `chun` → `model=huoshan&voice=chun` 正常合成
5. OpenAI 渠道 `openai-main` + 音色 `alloy` → 正常合成
6. **同名跨渠道**：`alloy` 在两个渠道各建一行 → 均可建、互不影响；**同渠道同名** → 409 `voice_duplicate`
7. 渠道 `default_voice='chun'` 不带 voice → 用 chun；`''` 且无 voice → 400 `voice_required`
8. OpenAI 请求 `ogg_opus` → 上游 `opus` → 回写 `Result.Format="ogg_opus"`，可播放
9. OpenAI 请求 `flac` → 降级 mp3，回写 `Result.Format="mp3"`
10. 双渠道并存互不串扰（火山音色名在 OpenAI 渠道不存在 → `unknown_voice`）
11. `default_voice` 悬空防护：改名同步、删除清空
12. `channel_id` 非法 → 400 `invalid_channel`；provider 空/未注册 → 400 `invalid_provider`
13. `channel.name` 含空格/斜杠/中文 → 400 `invalid_channel`
14. `GET /api/admin/providers` 返回已注册列表
15. 迁移：旧库升级 → voices 重建 + 复合索引 + `schema_version=7` 一次成功；模拟失败 → 版本不记账、下次重跑
16. **`/health` 在 normal 模式返回 200**（不再因缺全局配置 503）
17. `usage.model` = 渠道名；退款路径一致
18. Azure（真实账号）：wav 含 RIFF 头、pcm 无头、mp3 正常、24k 如实回报、`ogg_opus`→mp3
19. `go build` / `go vet` 全绿；`go test -race` 关键路径通过

---

## §13 分工

| 部分 | 负责 |
|---|---|
| 后端全部（store / adapter / controller / router 注册 / 文档） | 实现方 |
| 前端 `router/admin-channels.html`、`router/admin-voices.html` | 用户（沿用 STAGE4/5 分工） |
| Azure 真实账号验收 | 用户（无账号时交付骨架 + 单测） |

---

## §14 变更记录

| 版本 | 内容 |
|---|---|
| v1.5（本版） | 定案 G1–G9 + 补充定案（迁移 DROP+CREATE、`default_speaker` 忽略+WARN、channel 名正则、WAV 工具下沉、OpenAI/Azure 采样率与降级表）；删除 `SetupApply`；`dto.ConfigStatus` 移除；C11 去掉门户用途 |
| v1.4 | 吸收评审 H1/H2/M3–M7/S8–S10 |
| v1.3 | 12 条决策定案 + 三矛盾消解 |
| v1.2 | 渠道即模型（Channel-as-Model）|

---

## §15 实现完成与验证证据

### 15.1 静态与单测

| 项 | 结果 |
|---|---|
| `go build ./...` | ✅ |
| `go vet ./...` | ✅ |
| `go test ./...` | ✅ **27 PASS / 0 FAIL**（STAGE4 13 + STAGE5 11 + STAGE6 3） |

STAGE6 新增单测（`store/stage6_test.go`，按仓库约定 `*_test.go` 不提交）：

| 测试 | 锁死的缺陷 |
|---|---|
| `TestStage6VoicesRebuildRemovesGlobalUnique` | 旧 `name TEXT NOT NULL UNIQUE` 是**表级约束**，`DROP INDEX` 删不掉 ⇒ 同名跨渠道永远失败。断言：重建后历史音色被清空、同名跨渠道可插入、同渠道同名仍被拒 |
| `TestStage6MigrationIsVersionGated` | 门控若用 `schemaVersion` 而非 `stage6SchemaVersion`，将来升到 8 会**再次清空**管理员重建好的音色 |
| `TestStage6ChannelNameRejectsModelUnsafeChars` | `channel.name` 现在是对外 model 名，含空格/斜杠/中文的名字客户端**请求不出来** |

### 15.2 端到端 HTTP 实测（全新实例，不设任何凭证 env）

**29 PASS / 0 FAIL**

| 验收点 | 实测 |
|---|---|
| 无凭证 env 可启动 | ✅ 自动生成一次性安装 token |
| setup → 超管 | ✅ `200 {ok, owner, redirect}` |
| `/health` | ✅ **200**（修复前 normal 模式恒 503），响应已无 `config_status` |
| `GET /api/admin/providers` | ✅ `["azure","openai","volcano"]` |
| 建渠道：未注册 provider | ✅ `400 invalid_provider`（含已注册列表） |
| 建渠道：名字含空格 | ✅ `400 invalid_channel` |
| **同名音色跨渠道** | ✅ 两个渠道各建 `alloy` 均成功（**核心目标达成**） |
| 同渠道同名 | ✅ `409 voice_duplicate` |
| 不带 model | ✅ `400 model_required`，message 列出可用 model |
| 未知 model | ✅ `400 unknown_model` |
| 无 voice 且渠道无默认音色 | ✅ `400 voice_required` |
| 渠道内无该 voice | ✅ `400 unknown_voice` |
| 设渠道 `default_voice` 后不带 voice | ✅ 直接进入上游（不再 voice_required） |
| `default_voice` 指向不存在的音色 | ✅ `400 invalid_channel` |
| voice 按 id 删除 | ✅ `200`；不存在 → `404` |
| 余额为 0 时调用 | ✅ `402 insufficient_balance`（在路由通过之后） |
| 有余额时调用（假凭证） | ✅ 进入上游后 `500 synthesis_failed`（证明链路打通到 adapter） |
| **`usage.model`** | ✅ 记录 `huoshan`（**对外 model**，不是上游 model） |
| 双渠道不串扰 | ✅ `model=openai-main` 查已删音色 → `unknown_voice` |

### 15.3 实现期发现并修掉的问题

| # | 问题 | 处理 |
|---|---|---|
| 1 | `store/db.go` 基表 DDL 仍带表级 `UNIQUE(name)`，全新库一旦迁移 WARN 失败会**静默停留在全局唯一** | 基表 DDL 同步改新结构（G1） |
| 2 | `/health` 因 `GetTTSConfigErr()` 恒非 nil，在 normal 模式**永久 503**（STAGE5 遗留，K8s readiness 恒红） | 删掉整套"全局必填项"判定，503 只判 store 未就绪（B7/G9） |
| 3 | `TestStage5BackfillIsVersionGated` 靠"删 version=6 一行"回退，`schemaVersion` 升到 7 后门控失效 | 测试改为删除 `version >= 6`（`schema_version` 是累积行表） |
| 4 | 建渠道时 `ErrInvalid`（名字不合法）返回泛化的 `bad_request` | 翻成 `invalid_channel`，与错误码表一致 |

### 15.4 残余缺口（诚实记录）

- **真实上游未验证**：三个 adapter 均只用假凭证打到"进入上游并失败"，未跑通真实火山/OpenAI/Azure 的成功合成与计费结算。
- **Azure 无账号**：仅交付实现 + 编译 + 错误路径；wav/pcm/mp3 的真实音频未验听。
- **前端未做**（按 §13 分工归用户）：`admin-channels.html` 需加"渠道内音色管理 + provider 下拉 + `default_voice` + priority/weight 标注"；`admin-voices.html` 需改按 id 操作 + 渠道归属。
- **`priority`/`weight`/`auto_ban` 暂不生效**（B5）：字段保留，渠道页需显式标注，否则管理员会误以为配了有用。
- **`channel_group`/`user_group` 仍不参与路由**（STAGE5 起埋点，留给下一期"组隔离"）。
- `docs/UPSTREAM_ADAPTER_GUIDE.md` 尚未补 OpenAI/Azure 的完整范例章节（两个 adapter 源码已带详细注释，可先直接读代码）。
