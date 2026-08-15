package volcano

import (
	"encoding/binary"
	"fmt"
)

// 标准 PCM WAV 头(44 字节)。
// 文档 3.3 节:流式场景不推荐 wav(会多次返回 wav header),
// 本项目策略:上游走 pcm,本地拼一次标准头,避免拼接过个 header。
type wavHeader struct {
	// RIFF chunk descriptor
	ChunkID   [4]byte // "RIFF"
	ChunkSize uint32  // 36 + SubChunk2Size
	Format    [4]byte // "WAVE"
	// fmt sub-chunk
	Subchunk1ID   [4]byte // "fmt "
	Subchunk1Size uint32  // 16 for PCM
	AudioFormat   uint16  // 1 = PCM
	NumChannels   uint16
	SampleRate    uint32
	ByteRate      uint32
	BlockAlign    uint16
	BitsPerSample uint16
	// data sub-chunk
	Subchunk2ID   [4]byte // "data"
	Subchunk2Size uint32
}

// WrapWAVHeader 把 PCM 原始字节封装成完整的 WAV 字节流。
// sampleRate 决定 WAV 头里的采样率字段;pcm 视为 16-bit 单声道 little-endian。
func WrapWAVHeader(pcm []byte, sampleRate int) ([]byte, error) {
	if sampleRate <= 0 {
		return nil, fmt.Errorf("invalid sample rate %d", sampleRate)
	}
	const channels uint16 = 1
	const bitsPerSample uint16 = 16
	blockAlign := channels * bitsPerSample / 8
	byteRate := uint32(sampleRate) * uint32(blockAlign)
	dataSize := uint32(len(pcm))

	hdr := wavHeader{
		ChunkID:       [4]byte{'R', 'I', 'F', 'F'},
		ChunkSize:     36 + dataSize,
		Format:        [4]byte{'W', 'A', 'V', 'E'},
		Subchunk1ID:   [4]byte{'f', 'm', 't', ' '},
		Subchunk1Size: 16,
		AudioFormat:   1,
		NumChannels:   channels,
		SampleRate:    uint32(sampleRate),
		ByteRate:      byteRate,
		BlockAlign:    blockAlign,
		BitsPerSample: bitsPerSample,
		Subchunk2ID:   [4]byte{'d', 'a', 't', 'a'},
		Subchunk2Size: dataSize,
	}

	out := make([]byte, 0, 44+len(pcm))
	out = append(out, hdr.ChunkID[:]...)
	out = binary.LittleEndian.AppendUint32(out, hdr.ChunkSize)
	out = append(out, hdr.Format[:]...)
	out = append(out, hdr.Subchunk1ID[:]...)
	out = binary.LittleEndian.AppendUint32(out, hdr.Subchunk1Size)
	out = binary.LittleEndian.AppendUint16(out, hdr.AudioFormat)
	out = binary.LittleEndian.AppendUint16(out, hdr.NumChannels)
	out = binary.LittleEndian.AppendUint32(out, hdr.SampleRate)
	out = binary.LittleEndian.AppendUint32(out, hdr.ByteRate)
	out = binary.LittleEndian.AppendUint16(out, hdr.BlockAlign)
	out = binary.LittleEndian.AppendUint16(out, hdr.BitsPerSample)
	out = append(out, hdr.Subchunk2ID[:]...)
	out = binary.LittleEndian.AppendUint32(out, hdr.Subchunk2Size)
	out = append(out, pcm...)
	return out, nil
}
