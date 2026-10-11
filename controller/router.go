package controller

import (
	"sync/atomic"

	"github.com/volcano-tts/tts-api/adapter/route"
)

// routerPtr 持有当前可用的多渠道路由器;main 启动期调 SetRouter 注入,
// 阶段 2 管理接口上线后由 reloadChannels 整体替换。
//
// 阶段 2 改造(决策 #1):
//   - 用 atomic.Pointer 保证 Reload 写与 tts.go 热路径读之间无 data race
//   - 砍 SetRouter(nil) 语义:恒收非 nil(NewRouter([]) 与 nil 效果等价,
//     但 nil 多一个分支,阶段 2 没有"动态清空指针"需求——"全停/全删"走
//     channels 表状态即可触发兜底)
//   - GetRouter 恒返非 nil;调用方 `rtr.Empty()` 自然判定"零渠道"
var routerPtr atomic.Pointer[route.Router]

// SetRouter 注入 router 句柄;r 恒非 nil(空 router 也算,阶段 2 决策 #1)。
// main 启动期调一次,reloadChannels 每次写操作后调一次。
func SetRouter(r *route.Router) { routerPtr.Store(r) }

// GetRouter 拿当前 router 句柄;返回非 nil(主路径若不放心可再判 Empty())。
func GetRouter() *route.Router { return routerPtr.Load() }
