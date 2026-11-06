package volcano

import (
	"github.com/volcano-tts/tts-api/common"
)

// WrapWAVHeader 把 PCM 原始字节封装成完整的 WAV 字节流。
//
// STAGE6:实现已下沉到 common.WrapWAVHeader(Azure 的 pcm 剥头也需要 WAV 逻辑,
// 放在 adapter 之间互相 import 是错的)。这里保留同名函数作为**薄封装**,
// 避免改动既有测试与调用点。
func WrapWAVHeader(pcm []byte, sampleRate int) ([]byte, error) {
	return common.WrapWAVHeader(pcm, sampleRate)
}
