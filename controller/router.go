package controller

import (
	"github.com/volcano-tts/tts-api/adapter/route"
)

// routerInstance 持有当前可用的多渠道路由器;main 启动期调 SetRouter 注入。
// 阶段 1:启动期从 store.ChannelList 加载一次,运行期不变;阶段 2 管理接口上线后
// 会有 Reload 触发整体替换(届时 SetRouter 同样负责更新这个指针)。
//
// 注入失败 / 未注入时,GetRouter 返 nil —— 此时 OpenaiTTSHandler 走零渠道兜底路径
// (等价于"零 Channel 走 setting 默认 provider"),保持阶段 0 行为不变。
var routerInstance *route.Router

// SetRouter 注入 router 句柄;main 启动期调一次。
// 传入 nil 表示清空(回退到零渠道兜底),便于管理接口触发"禁用多渠道"操作(阶段 2)。
func SetRouter(r *route.Router) { routerInstance = r }

// GetRouter 拿当前 router 句柄;nil 表示未注入或被清空。
func GetRouter() *route.Router { return routerInstance }
