# 火山 TTS 聚合平台 · 项目书（v0.1）

> 定位：个人自用（无甲方）· 单机部署 · 单二进制分发
> 基线代码：Volcano-Engine-TTS-UI（develop 分支，Go，约 2900 行）

---

## 1. 项目概述

### 1.1 背景与动机

现有 `Volcano-Engine-TTS-UI` 是一个"火山引擎 TTS v3 → OpenAI 兼容接口"的适配器，已具备 `/v1/audio/speech`、多格式输出、鉴权限流、Prometheus 观测等能力。当前存在三个核心问题：

1. **配置全部堆在环境变量里**：API Key、资源 ID、音色、采样率……随音色增长会持续"溢出"，无法维护。
2. **音色是静态的**：`voice`/`model` 请求字段被接收但忽略，只支持 env 里配死的单音色。
3. **首次启动没有入口**：无 UI、无数据库，配置只能靠手写 env。

### 1.2 项目目标

把该项目升级为**自用聚合平台**：

- 用 **SQLite** 接管所有运行时可变配置（全局设置 + 音色库），环境变量只保留 3 个左右引导参数；
- 提供**引导式安装 UI**：首次启动（未安装）进入 `/setup` 向导收集配置，写入数据库后即完成安装；
- 采用 **lock 文件**作为安装状态判据，支持"损坏自动回退、可重置安装"；
- `/v1/audio/speech` 的 `voice` 参数**按数据库路由**，实现多音色动态切换；
- 保持**单二进制分发**（SQLite 用纯 Go 驱动，`//go:embed` 嵌入引导页）。

### 1.3 设计原则

| 原则 | 说明 |
|---|---|
| 数据库为唯一配置源 | 运行时的全局参数、音色全部读库，不做"库 + env 双轨" |
| 环境变量只做引导 | 仅保留 DB 路径、端口、初始化凭证 |
| lock 是安装门卫 | 存在 = 已安装；不存在 = 安装模式；损坏 = 备份 + 删 lock + 回退 |
| 损坏可自愈 | 库异常自动备份留档并回退安装模式，不裸奔 |
| 先内核后 UI | 本期只做"引导页"（安装必需），完整管理后台后置 |

---

## 2. 需求范围

### 2.1 本期（MVP）范围

| 编号 | 需求 | 说明 |
|---|---|---|
| R1 | SQLite 接入 | `modernc.org/sqlite`（纯 Go、无 CGO），自动建表、轻量迁移 |
| R2 | 全局配置入库 | `settings` 表接管现有全部 TTS 全局环境变量 |
| R3 | 音色库 | `voices` 表：name → speaker/resource_id/model/…，支持增删改查 |
| R4 | 安装状态检测 | `installed.lock` 判据 + 启动判定流程 |
| R5 | 引导式安装 UI | `/setup` 引导页（首次启动数据收集）+ `POST /api/setup` |
| R6 | 安装模式路由守卫 | 未安装时全站只开放 `/setup`，其余返回 503/跳转 |
| R7 | voice 动态路由 | `/v1/audio/speech` 按 `voice` 查库路由到火山 |
| R8 | 音色管理 API | `GET/POST /api/voices`、`PUT/DELETE /api/voices/:id`（带鉴权） |
| R9 | 损坏回退 | 库校验失败 → 备份 `.corrupt-<ts>` → 删 lock → 重新安装 |
| R10 | 环境变量收敛 | 迁移后 env 仅剩：`TTS_DB_PATH`、`PORT`、初始化凭证 |

### 2.2 后置（不在本期）

- 完整管理后台（音色列表/用量图表/配置管理）
- 多服务商聚合（火山 / OpenAI / 微软统一适配）
- 流式实时输出（SSE/WebSocket）
- TTS 结果缓存、长文本自动分片、字幕透出
- ASR 转写端点（`/v1/audio/transcriptions`）

---

## 3. 总体架构

```
┌───────────────────────────── 单二进制 tts-api ─────────────────────────────┐
│                                                                              │
│  main.go ── 启动：初始化 DB → 安装状态检测 → 加载配置 → 路由 → 监听            │
│                                                                              │
│  ┌────────────┐   ┌─────────────┐   ┌────────────────────────────────────┐   │
│  │ installer/ │──▶│   store/    │   │             router/                │   │
│  │ lock 检测  │   │ SQLite 访问 │   │  /setup(引导页·embed)  /api/setup  │   │
│  │ 安装模式   │   │ settings    │   │  /api/voices*        /v1/audio/speech│ │
│  └────────────┘   │ voices      │   │  /health /metrics /dashboard        │   │
│                   └─────────────┘   └────────────────────────────────────┘   │
│                                                                              │
│  controller/ ── 语音合成(查库路由) · setup · voices CRUD                      │
│  middleware/ ── 鉴权 · 限流 · 并发 · 安装守卫                                 │
│  setting/    ── 仅保留引导参数(DB路径/端口/凭证) + 启动汇总                    │
│  adapter/volcano/ ── 火山 v3 客户端（不改，仅入参来源变为 DB）                 │
└──────────────────────────────────────────────────────────────────────────────┘
```

---

## 4. 技术选型

| 项 | 选型 | 理由 |
|---|---|---|
| 语言 | Go（沿用） | 现有项目基线，无迁移成本 |
| 数据库 | SQLite（`modernc.org/sqlite`） | 纯 Go 无 CGO，保持 `CGO_ENABLED=0` 单二进制；单文件零运维 |
| 前端 | Vue3 + axios（沿用 dashboard 技术栈） | 复用现有 `//go:embed` 模式，`setup.html` 嵌入二进制 |
| 路由 | gorilla/mux（沿用） | 现有实现 |
| 构建 | 保持单二进制 | `//go:embed` 内嵌引导页与监控页 |

> 依赖注意：`modernc.org/sqlite` 体积较大（约 30-40MB 二进制），如介意可换 `mattn/go-sqlite3`（需 CGO，破坏单二进制），**本项目选前者**。

---

## 5. 数据模型设计

### 5.1 `settings` 表（全局配置）

```sql
CREATE TABLE IF NOT EXISTS settings (
    key        TEXT PRIMARY KEY,      -- 配置键名
    value      TEXT NOT NULL,          -- 配置值（统一存字符串，读取时按需转换）
    updated_at TEXT NOT NULL DEFAULT (datetime('now'))
);
```

初始化后必填的键：

| key | 说明 | 来源（原环境变量） |
|---|---|---|
| `api_key` | 火山 API Key | `BYTEDANCE_TTS_API_KEY` |
| `default_resource_id` | 默认资源 ID | `BYTEDANCE_TTS_RESOURCE_ID` |
| `default_speaker` | 默认音色 | `BYTEDANCE_TTS_SPEAKER` |
| `initialized` | 安装完成标记 `"1"` | —（双保险，配合 lock） |

可选键（读取时带默认值）：

| key | 默认值 | 来源 |
|---|---|---|
| `default_format` | `mp3` | `BYTEDANCE_TTS_FORMAT` |
| `sample_rate` | `24000` | `BYTEDANCE_TTS_SAMPLE_RATE` |
| `timeout` | `30s` | `BYTEDANCE_TTS_TIMEOUT` |
| `model` | 空 | `BYTEDANCE_TTS_MODEL` |
| `model_type` | 空 | `BYTEDANCE_TTS_MODEL_TYPE` |
| `explicit_language` | 空 | `BYTEDANCE_TTS_EXPLICIT_LANGUAGE` |
| `enable_subtitle` | `false` | `BYTEDANCE_TTS_ENABLE_SUBTITLE` |

> 迁移建议：首次安装时若检测到旧的对应环境变量仍存在，可作为引导页**预填默认值**（仅预填，不替代库），便于老用户平滑迁移。

### 5.2 `voices` 表（音色库）

```sql
CREATE TABLE IF NOT EXISTS voices (
    id          INTEGER PRIMARY KEY AUTOINCREMENT,
    name        TEXT NOT NULL UNIQUE,   -- 对外 voice 名，请求里 voice 字段查它
    speaker     TEXT NOT NULL,          -- 火山音色 ID（复刻音色以 S_ 开头）
    resource_id TEXT NOT NULL,          -- 对应火山资源 ID（决定计费/模型族）
    model       TEXT DEFAULT '',        -- 子模型 seed-tts-2.0-standard / -expressive
    language    TEXT DEFAULT '',        -- 显式语种（可选）
    description TEXT DEFAULT '',        -- 备注
    enabled     INTEGER NOT NULL DEFAULT 1,  -- 0/1 启用
    created_at  TEXT NOT NULL DEFAULT (datetime('now')),
    updated_at  TEXT NOT NULL DEFAULT (datetime('now'))
);
CREATE UNIQUE INDEX IF NOT EXISTS idx_voices_name ON voices(name);
```

> 默认音色：`settings.default_speaker` 表示请求**未传 voice** 时用的音色；也可以约定默认 voice 名为 `default` 的行，二选一，建议用 settings 字段（更直观）。

---

## 6. 安装与启动流程设计

### 6.1 lock 判据

- lock 文件：`<DB目录>/installed.lock`（与 `tts.db` 同目录）。
- **存在 = 已成功完成过安装**；不存在 = 未安装。
- lock 内容：一行文本 `version <版本号> <初始化时间>`，便于未来判断是否需要重装/迁移。
- 写入时机：**先写库、后写 lock**（`/api/setup` 成功后、且通过完整性校验后才原子创建 lock），避免"lock 在、库是半成品"。
- **不用 `.db` 文件是否存在作为安装判据**（建表会自动创建 db 文件，无法区分"从未安装"和"已安装"）。

### 6.2 启动判定流程

```
main 启动
  ├─ 解析引导环境变量（TTS_DB_PATH / PORT / 初始化凭证）
  ├─ store.Open()（打开/创建 SQLite，自动建表）
  │
  ├─ installed.lock 不存在？
  │    └─ YES → 进入【安装模式】：仅开放 /setup（静态资源 + 引导页 + POST /api/setup）
  │            其余路由（/v1/audio/speech、/api/voices 等）→ 503 + 跳转 /setup
  │    └─ NO  → 尝试读库 + PRAGMA integrity_check
  │              ├─ 通过 → 加载 settings/voices 到内存缓存 → 【正常模式】
  │              └─ 失败 → 备份 tts.db → tts.db.corrupt-<时间戳>
  │                        → 删除 installed.lock → 进入【安装模式】
  │
  └─ 打印启动摘要（模式 / DB路径 / lock状态 / 音色数）
```

### 6.3 损坏回退规则

1. 只在 lock 存在但库打不开 / `integrity_check` 失败时触发回退；
2. **先备份**：损坏的 `.db` 改名 `tts.db.corrupt-<ts>` 留档，不直接删除；
3. 删除 `installed.lock`，进入安装模式；
4. 日志明确打印回退原因（文件损坏 / 权限 / 磁盘 / 版本等），便于排查；
5. 回退后用户重新走 `/setup` 即可恢复。

### 6.4 setup 劫持防护（必须）

安装模式是"谁先访问谁配置"，公网暴露时存在被抢先初始化的风险。防护方案（至少一项）：

- **A（推荐）初始化凭证**：启动时打印一次性 setup token（或从环境变量 `TTS_ADMIN_KEY` 指定），引导页提交时必须带 token，校验通过才写入；
- **B 回环限制**：安装模式下 `/setup` 仅允许本机回环地址访问（`127.0.0.1`），初始化完成后即失效；
- 二者可叠加。初始化完成后 `/api/setup` 永久关闭。

---

## 7. 接口设计

### 7.1 安装相关

| 方法 | 路径 | 说明 | 鉴权 |
|---|---|---|---|
| GET | `/setup` | 引导页 HTML（`//go:embed`） | 安装模式开放 |
| GET | `/api/setup/status` | 返回 `{installed: bool}`，供引导页判断 | 无 |
| POST | `/api/setup` | 提交初始配置（全局 + 音色列表 + token）→ 写库 → 写 lock | 初始化凭证 |

`POST /api/setup` 请求体示例：

```json
{
  "token": "一次性凭证",
  "settings": {
    "api_key": "xxx",
    "default_resource_id": "volc.megatts.default",
    "default_speaker": "zh_female_qingxin",
    "default_format": "mp3",
    "sample_rate": 24000
  },
  "voices": [
    { "name": "qian",  "speaker": "S_xxx", "resource_id": "volc.megatts.icl", "model": "seed-tts-2.0-standard" },
    { "name": "xun",   "speaker": "S_yyy", "resource_id": "volc.megatts.icl", "model": "seed-tts-2.0-expressive" }
  ]
}
```

响应：`200 {ok:true, message:"installed"}` 或 `400/401/409`。

### 7.2 音色管理（正常模式）

| 方法 | 路径 | 说明 |
|---|---|---|
| GET | `/api/voices` | 列表（可分页/过滤 enabled） |
| POST | `/api/voices` | 新增音色（name 唯一冲突返回 409） |
| PUT | `/api/voices/:id` | 更新音色 |
| DELETE | `/api/voices/:id` | 删除音色（`default_speaker` 引用的音色禁止删除） |

鉴权：复用现有 `OPENAI_TTS_API_KEY`（Bearer）。若安装后用户未配置管理 Key，可提示在 settings 中配置。

### 7.3 语音合成（改造点）

`/v1/audio/speech` 改造逻辑：

```
收到请求
  ├─ voice 为空 → 用 settings.default_speaker（无默认 → 400 "no default voice"）
  ├─ voice 非空 → 查 voices 表
  │    ├─ 命中 → 用该行 speaker/resource_id/model/language 覆盖 opts
  │    └─ 未命中 → 400 "unknown voice: <name>"
  ├─ model 非空 → 查库/映射（本期：model 仅校验长度，不做映射，或按 settings 默认）
  └─ 其余逻辑不变（格式解析、speed、鉴权、限流、埋点）
```

### 7.4 现有端点（不变）

`/health`、`/metrics`、`/dashboard`、`/` 行为保持；但**安装模式下**除 `/setup` 外统一返回 503（`/health` 可返回 `installed:false` 便于部署探针识别未初始化）。

---

## 8. 模块划分与代码改动清单

### 8.1 新增包

| 包 | 职责 | 关键文件 |
|---|---|---|
| `store/` | SQLite 访问层：打开/建表/迁移、settings CRUD、voices CRUD、integrity_check | `db.go`、`settings.go`、`voices.go`、`migrate.go` |
| `installer/` | 安装状态：lock 检测/创建/删除、安装模式判定、损坏回退 | `lock.go`、`bootstrap.go` |

### 8.2 改造文件

| 文件 | 改动 |
|---|---|
| `router/router.go` | 新增 `/setup`、`/api/setup/*`、`/api/voices`；安装模式路由守卫 |
| `controller/` | 新增 `setup.go`（安装提交）、`voices.go`（CRUD）；改造 `tts.go`（voice 查库路由） |
| `middleware/` | 新增 `installguard.go`（安装模式拦截，未安装非 `/setup` → 503） |
| `setting/config.go` | 收敛：仅读 `TTS_DB_PATH`、`PORT`、初始化凭证；启动汇总展示"模式/lock/音色数" |
| `main.go` | 启动流程：初始化 DB → 安装检测 → 模式分支 |
| `router/setup.html` | 新增引导页（Vue3 + axios，`//go:embed`） |
| `go.mod` | 新增 `modernc.org/sqlite` |
| `.env.example` / `README.md` / `docker-compose.yml` | 更新为新的引导参数与首次安装说明 |

### 8.3 环境变量收敛表

**迁移前（现状，会持续膨胀）：**

```
BYTEDANCE_TTS_API_KEY
BYTEDANCE_TTS_RESOURCE_ID
BYTEDANCE_TTS_SPEAKER
BYTEDANCE_TTS_FORMAT
BYTEDANCE_TTS_SAMPLE_RATE
BYTEDANCE_TTS_BIT_RATE
BYTEDANCE_TTS_MODEL
BYTEDANCE_TTS_MODEL_TYPE
BYTEDANCE_TTS_EXPLICIT_LANGUAGE
BYTEDANCE_TTS_ENABLE_SUBTITLE
BYTEDANCE_TTS_TIMEOUT
BYTEDANCE_TTS_DEBUG
OPENAI_TTS_API_KEY
ALLOWED_ORIGINS
TRUSTED_PROXY_HOPS
PORT
```

**迁移后（仅引导参数，其余进库）：**

```
TTS_DB_PATH        # 数据库/lock 目录（默认 ./）
PORT               # 监听端口
TTS_ADMIN_KEY      # 安装初始化凭证（可选，不设则启动打印一次性 token）
OPENAI_TTS_API_KEY # 管理 API / 合成 API 鉴权（可选，迁移进 settings 或保留）
ALLOWED_ORIGINS    # CORS（可保留，属运行环境而非业务配置）
TRUSTED_PROXY_HOPS # 反代拓扑参数（保留，属部署环境）
```

> `BYTEDANCE_TTS_DEBUG`、`TRUSTED_PROXY_HOPS`、`ALLOWED_ORIGINS` 属"部署/运维环境"而非"业务配置"，可留在 env；其余 TTS 业务配置全部进库。

---

## 9. 安全设计

| 项 | 措施 |
|---|---|
| Setup 劫持 | 初始化凭证（`TTS_ADMIN_KEY` 或一次性 token）+ 可选回环限制；完成后 `/api/setup` 永久关闭 |
| 合成/管理鉴权 | 复用 `OPENAI_TTS_API_KEY`（Bearer）；voice 路由不绕过鉴权 |
| 敏感信息 | API Key 在引导页只进不出；日志/`/health` 不回显明文 Key（沿用 `maskAPIKey`） |
| 输入校验 | voice 名白名单（字母数字 `_-`）、长度限制；SQL 全部参数化，防注入 |
| lock/DB 写入 | 先写库后写 lock；lock 原子创建；损坏先备份再回退 |
| 默认音色保护 | 删除被 `default_speaker` 引用的音色时拒绝（409） |

---

## 10. 开发计划与里程碑

| 里程碑 | 内容 | 验收标准 |
|---|---|---|
| M1 存储层 | `store/` 包：SQLite 接入、两表建表、迁移、settings/voices CRUD、`integrity_check` | `go build` 通过；单测覆盖 CRUD |
| M2 安装流程 | `installer/`：lock 检测/创建/删除、安装模式判定、损坏回退；`middleware/installguard` | 无 lock → 安装模式；有 lock → 正常模式；损坏库 → 备份+回退 |
| M3 引导 UI | `/setup` 引导页 + `POST /api/setup`（token 校验、写库、写 lock） | 首次访问可完成安装；重复安装被拒；token 错误 401 |
| M4 音色路由 | `/api/voices` CRUD + `/v1/audio/speech` voice 查库路由 | 新增音色后 voice 生效；未知 voice 400；默认音色兜底 |
| M5 收敛与文档 | `setting/` 收敛、`.env.example`/README/部署更新、启动摘要展示模式与音色数 | 迁移后仅引导 env；文档与行为一致 |
| M6 测试收尾 | 覆盖 lock 判定、setup 流程、voice 路由、损坏回退的集成测试 | 关键路径有自动化测试 |

建议 M1→M2 连续做（安装流程是主链路），M3 与 M2 可并行；M4 依赖 M1 完成。

---

## 11. 风险与对策

| 风险 | 影响 | 对策 |
|---|---|---|
| `modernc.org/sqlite` 体积增大 | 单二进制从 ~7MB 增至 ~40MB | 接受；如不可接受改 CGO 版（牺牲单文件） |
| Setup 劫持（公网部署） | 他人抢先配置 | 初始化凭证 + 回环限制 + 完成后关闭端点（见 6.4） |
| 库损坏导致服务不可用 | 服务起不来 | 自愈回退：备份 + 删 lock + 重新安装（见 6.3） |
| 老用户迁移 | 现有 env 用户升级后无库 | 引导页预填旧 env 值；README 给出迁移步骤 |
| voice 名冲突/默认引用 | 删除默认音色致不可用 | 唯一约束 + 默认音色删除保护（409） |
| 并发写库 | 数据竞争 | 单写锁（`database/sql` 默认 + 业务层互斥），单用户场景风险低 |
| 表结构未来升级 | 旧库不兼容 | lock 内容带版本号；`migrate.go` 预留版本迁移 |

---

## 12. 验收标准（本期 MVP）

1. 删除所有业务 env 后，首次启动进入 `/setup` 引导页，可完成安装（全局 + ≥1 音色）；
2. 安装完成后有 `installed.lock`，重启进入正常模式，`/v1/audio/speech` 可用；
3. 通过 `/api/voices` 新增音色后，请求带该 `voice` 能正常合成；未知 voice 返回 400；
4. 未传 `voice` 时使用默认音色；
5. 手动制造损坏库 → 自动备份 `.corrupt-*` 并删 lock 回退安装模式；
6. 安装模式下 `/v1/audio/speech` 返回 503 或跳转 `/setup`；
7. 单二进制运行，无外部文件依赖（引导页已 embed）。

---

## 13. 非目标（明确不做）

- 本期不做多服务商聚合、流式输出、缓存、字幕透出、ASR；
- 不做完整的运营管理后台（仅引导页，管理 API 先行）；
- 不做多租户/多用户体系（个人自用，单管理员）。

---

*文档版本：v0.1 · 状态：待评审 · 配套代码基线：Volcano-Engine-TTS-UI @ develop*
