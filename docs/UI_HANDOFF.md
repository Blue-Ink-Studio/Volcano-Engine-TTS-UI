# UI 外包 Handoff · 火山 TTS 管理后台

> 历史文档。本文档描述的是**外包给 Codex 做视觉改造**那一次任务,目标是"现有功能完全不动, 只换皮"。
>
> ⚠️ **状态: 任务已结束, 且路线已变更。** 该任务之后, admin 后台由提交 `9245c13`
> 自行重构成**多页 SPA**(见下面 §0.1),本文档正文里的"单文件 / 大小上限 / 改这两个 .html"
> 等约束**均已失效**,保留仅为记录当时的交接内容与设计讨论。
> 若要再做视觉改造,请以 §0.1 的当前文件结构为准。

---

## 0.1 当前真实结构(以此为准)

admin 后台已从单文件拆分为多页面,共享一套 CSS/JS,由 `//go:embed` 逐条嵌入:

| 文件 | 作用 | 大小 |
|---|---|---|
| `router/admin.html` | 后台外壳(导航 / 布局) | 5.5KB |
| `router/admin-login.html` | 登录页 | 2.1KB |
| `router/admin-voices.html` | 音色管理页 | 13.5KB |
| `router/admin-settings.html` | 设置页 | 14.4KB |
| `router/admin.css` | 共享样式(全部页面) | 20.2KB |
| `router/admin-shell.js` | 共享脚本: axios 实例 / toast / 外壳挂载 / 401 拦截 | 7.1KB |
| `router/setup.html` | 安装引导页(仍是单文件) | 24.0KB |

- URL 路由(非 hash):`/admin` · `/admin/login` · `/admin/voices` · `/admin/settings`
- 公共逻辑集中在 `admin-shell.js`,通过 `window.admin` 暴露
  `{ http, getKey, setKey, clearKey, toast, mountShell, formatUptime, … }`
- **已无"单文件 ≤ 35KB"这类约束**;新增页面请复用 `admin.css` + `admin-shell.js`,不要各写一套。

---

## 0. 任务概览(历史原文)

| 项 | 说明 |
|---|---|
| 改的文件 | `router/admin.html`、`router/setup.html` |
| 改的部分 | 视觉、布局、组件、交互模式（loading/empty/error/反馈）|
| 不改的部分 | API 调用、状态管理逻辑、字段名、文案语义 |
| 期望产物 | 改完后的两个 .html, 总大小 ≤ 64KB（admin ≤ 35KB, setup ≤ 15KB）|
| 验收 | dev 起服, 浏览器走完所有流程无回归 + 视觉明显更好 |

---

## 1. 技术栈约束（历史原文；"单文件"部分已失效）

| 项 | 现状 | 改动空间 |
|---|---|---|
| 构建 | **无构建步骤**（`//go:embed` 进 binary）| 不可改 |
| Vue | 3.4 via `cdn.bootcdn.net` | 可换版本（保持 3.x 即可）|
| HTTP | axios 1.6（集中在 `admin-shell.js`）| 可换（fetch / ky / 留 axios 都行）|
| CSS | 手写, 走 CSS 变量, 集中在 `admin.css` | 可换 **Tailwind CDN / DaisyUI / Pico / 手写** 都行 |
| 图标 | 暂无 | 可加 lucide / heroicons inline SVG / emoji |
| 字体 | 系统默认 (`-apple-system, "PingFang SC"...`) | 可加 `Noto Sans SC` via Google Fonts, 但要评估大小 |
| 路由 | 拆分后已改为 **URL 路由**（`/admin/voices` 等）,不再是 hash 三 tab | 保持 URL 路由 |

**注意（历史约束,已不适用）**: 当时二进制大小敏感 —— admin.html 31.19KB / setup.html 13.01KB。拆分后 admin 侧总量约 63KB(分散在 6 个文件里,浏览器可分页只加载所需部分),embed 进 binary 后大小影响可接受。**仍然建议**: 不要无脑加 UI 库(Tailwind 全量 + DaisyUI 会显著增大 embed 体积);要加就加在 `admin.css` 一处,所有页面共享。

---

## 2. 路由 & 模式

### 2.1 三个 URL
- `GET /setup` → `router/setup.html`（安装模式才进得去）
- `GET /admin` → `router/admin.html`（任何模式都返回 HTML, 鉴权由前端 JS 拦截）
- `GET /` → 重定向到 `/setup` 或 `/admin`

### 2.2 状态机（admin.html）

```
未登录 (无 sessionStorage 'ttsAdminKey')
  ├─ 输入 key + 调 GET /api/admin/overview 验证
  └─ 验证通过 → 写入 sessionStorage + 切到登录后视图

已登录
  ├─ 三个 tab: dashboard / voices / settings（hash 路由）
  ├─ 调任意 API 返 401 → 清 sessionStorage + 回到登录页
  └─ 改完自己 auth_key → 调 PUT /api/settings/auth-key 200 后清 sessionStorage
```

### 2.3 状态机（setup.html）

```
未提交: 表单 4 必填 + voices 至少 1 条
  └─ onMounted 调 GET /api/setup/prefill 自动填默认值
  └─ 提交 POST /api/setup, 成功后 location.href = '/admin'
```

---

## 3. API 契约（前端用的全部）

### 3.1 鉴权

| 头 | 说明 |
|---|---|
| `Authorization: Bearer <key>` | 所有 /api/* 请求必带（除 /api/setup/*）|
| 401 响应 | 清 sessionStorage `ttsAdminKey`, 回到登录页 |

**axios 实例已配置**:
```js
const http = axios.create({ baseURL: '/api' });
http.interceptors.request.use(c => {
  if (apiKey.value) c.headers.Authorization = 'Bearer ' + apiKey.value;
  return c;
});
http.interceptors.response.use(r => r, err => {
  if (err.response && err.response.status === 401) {
    sessionStorage.removeItem('ttsAdminKey');
    apiKey.value = '';
  }
  return Promise.reject(err);
});
```

### 3.2 端点

| Method | Path | 请求 body | 响应 | 鉴权 |
|---|---|---|---|---|
| GET | `/api/setup/prefill` | — | `{ settings: {default_resource_id, default_speaker, default_format, sample_rate} }` | 否（仅 install 模式）|
| POST | `/api/setup` | `{token, settings: {auth_key, api_key, default_resource_id, default_speaker, default_format, sample_rate}, voices: [{name, speaker, resource_id, model, language}]}` | `{ok, redirect, settings, voices}` | 否（仅 install 模式）|
| GET | `/api/admin/overview` | — | `{mode, installed, db_path, lock_path, version, commit, uptime_seconds, start_time, voice_count, voice_enabled_count, memory: {goroutines, heap_alloc, heap_inuse}}` | 是 |
| GET | `/api/admin/metrics` | — | Prometheus text | 是 |
| GET | `/api/voices` | — | `{voices: [{ID, Name, Speaker, ResourceID, Model, Language, Description, Enabled, CreatedAt, UpdatedAt}]}` | 是 |
| POST | `/api/voices` | `{name, speaker, resource_id, model, language?, description?}` | 创建的 voice 对象 | 是 |
| DELETE | `/api/voices/{name}` | — | `{deleted, ok}` | 是 |
| PATCH | `/api/voices/{name}/toggle` | `{enabled: bool}` | 更新后的 voice | 是 |
| GET | `/api/settings` | — | 完整 settings 对象（见 3.3）| 是 |
| PUT | `/api/settings` | 部分字段（`default_resource_id` / `default_speaker` / `default_format` / `sample_rate` / `model`）| `{ok, updated, updated_at}` | 是 |
| PUT | `/api/settings/api-key` | `{api_key: string}` | `{ok}` | 是 |
| PUT | `/api/settings/auth-key` | `{auth_key: string}` | `{ok}` | 是 |
| PUT | `/api/settings/cors` | `{allow_all?: bool, origins?: string}` | `{ok, allow_all, origins, cors_active}` | 是 |

### 3.3 GET /api/settings 完整响应

```json
{
  "api_key": "volc****etup",       // 打码
  "api_key_set": true,
  "auth_key": "my-o****etup",       // 打码
  "auth_key_set": true,
  "cors_allow_all": false,
  "cors_origins": "https://app.example.com\nhttps://other.com",
  "cors_configured": true,          // banner 显示控制
  "default_resource_id": "volc.megatts.icl",
  "default_speaker": "qian",
  "default_format": "mp3",
  "sample_rate": 24000,
  "model": "seed-tts-2.0-standard",
  "model_type": 0,
  "explicit_language": "",
  "enable_subtitle": false,
  "updated_at": "2026-08-29T16:07:49Z"
}
```

### 3.4 错误响应格式

所有错误（除 OpenAI 兼容的 /v1/audio/speech）用同一格式:
```json
{ "error": { "code": "bad_request", "message": "...", "type": "invalid_request_error" } }
```

前端用 `e.response?.data?.error?.message` 提取.

---

## 4. 字段语义（写文案 / placeholder 用）

### 4.1 auth_key
"OpenAI 鉴权 Key"
- 客户端用这个 Key 调 `/v1/audio/speech`
- 登录 `/admin` 也用这个 Key
- 自定字符串, 不是火山给的, **完全自己造**
- 例: `my-secret-key-2024`

### 4.2 api_key
"火山 TTS API Key" / "上游 API Key"
- 调火山 v3 上游用的凭证
- 从 https://console.volcengine.com/ 获取
- 例: `5b4d7c2a-...`

### 4.3 default_resource_id
- 音色所属的计费资源 ID
- `volc.megatts.default` 默认音色 / `volc.megatts.icl` 复刻音色

### 4.4 default_speaker
- 未传 `voice` 字段时用
- 必须是"音色" tab 里已添加的 voice name

### 4.5 voices 字段
- `name`: 短英文 id, 唯一, 不可改
- `speaker`: 火山给的 speaker id
- `resource_id`: 同上 default_resource_id
- `model`: `seed-tts-2.0-standard` (默认) / `seed-tts-2.0-expressive` / `seed-tts-2.0-clone`
- `language`: ISO 639-1 (`zh` / `en` / `ja` 等)

### 4.6 CORS origins
- 一行一个 origin
- 必须 `http://` 或 `https://` 开头
- 末尾 `/` 自动 trim

---

## 5. 当前 UI 的具体问题（按痛感排序）

### 5.1 admin.html
| # | 问题 | 现状 | 期望 |
|---|---|---|---|
| 1 | **登录页太空** | 一个输入框 + 一个按钮 | 加 logo / 提示 / 服务名 / 安装状态提示 |
| 2 | **dashboard 卡片平** | 4 个数字 + 路径文本, 无图标无图表 | 加 icon, 用 chart mini sparkline (uptime / memory)|
| 3 | **voices 表格简陋** | 5 列 + 启停 switch + 删除 | 加排序/搜索/批量操作/分页/拖拽改顺序 |
| 4 | **新增 voice 弹窗原始** | 6 个 input 堆叠 | 分组, 加 help text, 加示例值, 加实时校验 |
| 5 | **设置页 1 长页** | 4 段串成 1 长 list | 分 tab/分卡片, 加描述/帮助链接/危险操作分离 |
| 6 | **改 key 后无 feedback** | 改了直接清 session | 加 toast 通知 "Key 已更新, 请用新 Key 重新登录" |
| 7 | **删除无 undo** | confirm 弹原生 confirm() | 加 inline 二次确认 / undo snackbar |
| 8 | **loading 无** | 调 API 时按钮不变 | 加 spinner / disabled 状态 / 进度条 |
| 9 | **error 无专门显示区** | 用 `actionErr.value` 全局串 | 用 toast / inline error / 错误码对照 |
| 10 | **响应式差** | desktop only | 至少平板能用, 关键操作 (tab 切换) 移动端可点 |
| 11 | **配色单调** | 1 个 cyan 强调色 | 加二级色, success/warning/danger 用得不够明显 |
| 12 | **字体单薄** | 14px 全局 | 大字标题 24-32px, 重要数字 monospace 突出 |

### 5.2 setup.html
| # | 问题 | 现状 | 期望 |
|---|---|---|---|
| 1 | **4 字段加 voice 列表一锅烩** | 一长页 | 分步骤 (1.凭证 2.默认 3.音色 4.确认) |
| 2 | **voice 行无复用 UI** | 自己手写的 row | drag-to-reorder + 复制/粘贴/从预置模板加 |
| 3 | **无进度感** | 一个 "安装" 按钮 | 安装中显示进度 (写 settings → 写 voices → 写 lock) |
| 4 | **无"安装后会发生什么"** | 提交后只跳 /admin | 加结果页: 装了什么 / 接下来去 admin 干啥 |
| 5 | **无 reset** | 改了一锅填错没法批量重置 | "恢复默认值" 按钮 |
| 6 | **移动端糟糕** | 1 长页 | 至少 step indicator 在窄屏可见 |

### 5.3 共性问题
| # | 问题 | 期望 |
|---|---|---|
| 1 | 无 dark/light toggle | 加 toggle, 默认跟随系统 |
| 2 | 无键盘快捷键 | `/` 聚焦搜索, `Esc` 关弹窗, `Ctrl+S` 保存当前表单 |
| 3 | 无国际化 | 默认 zh-CN, 可加 i18n 但不强制 |
| 4 | 无 a11y 标签 | aria-label, focus ring, 键盘可达 |

---

## 6. 设计参考（建议风格, 不是强制)

| 风格 | 适配场景 | 推荐度 |
|---|---|---|
| Linear / Vercel dashboard | 后台工具, dark mode 友好 | ⭐⭐⭐⭐⭐ |
| Tailscale admin | 简洁, 卡片化, 信息密度高 | ⭐⭐⭐⭐ |
| Coolify / CasaOS | 自托管, "家用服务器"感 | ⭐⭐⭐ |
| shadcn/ui | 现代 React 风, 移植到 Vue 略费劲 | ⭐⭐⭐ (参考设计, 不直接用) |
| Tailadmin | 通用 admin 模板, 选段借鉴 | ⭐⭐ |

**主调色建议** (如果重做):
- 背景: 深色 (`#0a0e1a`) 或浅色 (`#f8fafc`), 配 system pref
- 强调色: cyan / indigo / emerald 之一
- 字体: 标题用 Inter, 数字用 JetBrains Mono

---

## 7. 不要做的事

- ❌ 改 API 路径 / 改字段名 / 改字段语义
- ❌ 加任何状态管理库 (Pinia) — 单文件 setup() 够用
- ❌ 加 webpack/vite — 必须保持单文件
- ❌ 引大 UI 库 (Element Plus / Naive UI) — 单文件塞不下
- ❌ 把 admin.html 和 setup.html 合并 — 它们是独立页面
- ❌ 改 axios 实例的 baseURL 或 401 拦截逻辑
- ❌ 改 hash 路由的 key 命名（`#dashboard` / `#voices` / `#settings`）
- ❌ **不要把单文件架构当最终形态** — 这是最小可工作版本(MVP),看板有 `ui-arch-debt` 卡片记录拆分计划。Codex 改造时保持单文件以便本次走通,但**不要引入新依赖让单文件更难拆**。例如:不要把 axios 拦截器写得依赖 10 个全局 ref;不要让 CSS 选择器深嵌套到无法抽离。

---

## 8. 验收清单（已按拆分后的结构更新）

改完回来, 我会逐项过:

- [ ] `go build ./...` 通过
- [ ] `go vet ./...` 通过
- [ ] `go test ./... -count=1` 全绿
- [ ] dev 起服后浏览器:
  - [ ] `/setup` 流程能跑完
  - [ ] `/admin` 登录 → dashboard → voices → settings → 增改删全通
  - [ ] 改 auth_key 后自踢 → 重新登录 → 能用新 key
  - [ ] CORS 未配时顶部 banner 出现, 配后消失
  - [ ] 401 错误时自动跳回登录
- [ ] 视觉: 跟现在的 admin 比有明显提升 (logo / 排版 / 颜色 / 反馈)
- [ ] 至少在 1024px 宽度下好看
- [ ] 没有 console error / 404
- [ ] 新增页面复用了 `admin.css` + `admin-shell.js`,没有复制粘贴出第二套公共逻辑

> 旧的"`admin.html` ≤ 35KB / `setup.html` ≤ 15KB"两条**已作废**:admin 已拆成多页,
> 单文件大小不再是约束。

---

## 9. 回填方式（历史原文） / 当前做法

历史原文:

```
1. 拿回 router/admin.html + router/setup.html
2. 放到 D:\项目\Volcano-Engine-TTS-UI\router\
3. 我 build + 测, 通过后 commit
4. push, 服务器自动部署
```

拆分之后的做法:改动落在 §0.1 表格里对应的**具体文件**上(外壳/登录/音色/设置,以及共享的
`admin.css` / `admin-shell.js`),不再有"两个 .html"这一说。改完在**项目里**跑:

```
go build ./...
go vet ./...
go test ./... -count=1
```

三条全过再 commit 到 `develop`。

---

## 10. 关联文件（不需要改, 但可读懂逻辑）

- `controller/admin.go` — admin API 实现
- `controller/settings.go` — settings API 实现
- `controller/setup.go` — setup API 实现
- `router/router.go` — Go 路由 + 挂载点（看 `//go:embed` 用法）
- `router/admin-shell.js` — 共享 axios 实例 / toast / 外壳挂载（改交互先看这里）
- `middleware/admin_auth.go` — RequireAdmin 实现（理解 401 怎么来）

---

## 11. 当前文件位置（已更新）

| 文件 | 大小 |
|---|---|
| `router/admin.html` | 5.5KB |
| `router/admin-login.html` | 2.1KB |
| `router/admin-voices.html` | 13.5KB |
| `router/admin-settings.html` | 14.4KB |
| `router/admin.css` | 20.2KB |
| `router/admin-shell.js` | 7.1KB |
| `router/setup.html` | 24.0KB |

`ui-handoff/` 目录里保留的是**改造前**的单文件副本与离线 mock 预览,仅供对照,不要回填。
