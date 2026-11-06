// Package common 提供跨包共享的小工具(常量、WAV 封装等)。
package common

import (
	"encoding/binary"
	"errors"
	"fmt"
)

// WAV 头长度(标准 PCM 44 字节)。
const wavHeaderSize = 44

// WrapWAVHeader 把 PCM 原始字节封装成完整的 WAV 字节流。
// sampleRate 决定 WAV 头里的采样率字段;pcm 视为 16-bit 单声道 little-endian。
//
// 上游策略(火山):流式场景不推荐直接要 wav(会多次返回 wav header),
// 所以本项目统一"上游走 pcm → 本地拼一次标准头"。
// STAGE6 起 Azure 也复用本函数(它的 riff-* 格式已带头,反向用 StripWAVHeader)。
func WrapWAVHeader(pcm []byte, sampleRate int) ([]byte, error) {
	if sampleRate <= 0 {
		return nil, fmt.Errorf("invalid sample rate %d", sampleRate)
	}
	const channels uint16 = 1
	const bitsPerSample uint16 = 16
	blockAlign := channels * bitsPerSample / 8
	byteRate := uint32(sampleRate) * uint32(blockAlign)
	dataSize := uint32(len(pcm))

	out := make([]byte, 0, wavHeaderSize+len(pcm))
	out = append(out, 'R', 'I', 'F', 'F')
	out = binary.LittleEndian.AppendUint32(out, 36+dataSize)
	out = append(out, 'W', 'A', 'V', 'E')
	out = append(out, 'f', 'm', 't', ' ')
	out = binary.LittleEndian.AppendUint32(out, 16) // Subchunk1Size
	out = binary.LittleEndian.AppendUint16(out, 1)  // PCM
	out = binary.LittleEndian.AppendUint16(out, channels)
	out = binary.LittleEndian.AppendUint32(out, uint32(sampleRate))
	out = binary.LittleEndian.AppendUint32(out, byteRate)
	out = binary.LittleEndian.AppendUint16(out, blockAlign)
	out = binary.LittleEndian.AppendUint16(out, bitsPerSample)
	out = append(out, 'd', 'a', 't', 'a')
	out = binary.LittleEndian.AppendUint32(out, dataSize)
	out = append(out, pcm...)
	return out, nil
}

// StripWAVHeader 从一段 WAV 字节流里剥出裸 PCM(去掉 RIFF 头)。
//
// 用途:Azure 只能输出带 RIFF 头的 `riff-24khz-16bit-mono-pcm`,
// 而客户端要的是裸 pcm,所以在 adapter 里剥掉。
//
// 实现上不假设头一定是 44 字节 —— 真实 WAV 可能有额外的 chunk(LIST/fact 等),
// 因此按 chunk 逐个走,找到 `data` 才切。只有严格的 "RIFF....WAVE" 才认定是 WAV;
// 不是 WAV 就原样返回(容忍上游直接给裸 PCM 的情况)。
func StripWAVHeader(wav []byte) ([]byte, error) {
	if len(wav) < 12 || string(wav[0:4]) != "RIFF" || string(wav[8:12]) != "WAVE" {
		// 不是 WAV:当作已经是裸 PCM 原样返回(不报错,避免误杀)
		return wav, nil
	}
	// 从 12 开始遍历 chunk
	off := 12
	for off+8 <= len(wav) {
		id := string(wav[off : off+4])
		size := int(binary.LittleEndian.Uint32(wav[off+4 : off+8]))
		body := off + 8
		if id == "data" {
			end := body + size
			if size <= 0 || end > len(wav) {
				// data 长度字段不可信(流式/截断)时,取到末尾
				end = len(wav)
			}
			return wav[body:end], nil
		}
		// chunk 按 2 字节对齐
		off = body + size + size%2
	}
	return nil, errors.New("wav: data chunk not found")
}
