// controller/seed.go — 【STAGE5 已退役,本文件不再含可执行代码】
//
// 原职责:启动期把 `OPENAI_TTS_API_KEY` 里的每个 key seed 成
// 「默认 user `default`」下的一条 unlimited token。
//
// 为什么退役(STAGE5 契约 §8):
//   - `OPENAI_TTS_API_KEY` 退役,业务调用方一律改用**门户自建的用户 token**;
//   - 当前无生产环境 ⇒ 不做任何过渡兼容(不留"env 存在就 seed 一次"的旁路)。
//
// 为什么是**整段删除**而不是留一个"key 列表为空时自然 no-op"的实现:
// 遗留的 env 会让它凭空造出一个 `default` 用户 + unlimited token,
// 而 STAGE5 的 owner 保障逻辑(`store.EnsureOwnerExists`)会把"最早的 is_admin 历史用户"
// 提为 owner —— 于是一个**环境变量**就能决定谁是超管。这是必须避免的隐性权限来源。
//
// 业务令牌的创建入口现在是:`POST /api/me/tokens`(controller/me.go)。
package controller
