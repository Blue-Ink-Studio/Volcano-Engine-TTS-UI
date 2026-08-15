package volcano

import (
	"bufio"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"time"

	"github.com/volcano-tts/tts-api/dto"
)

// ParsedStream 是一次流式响应的累计结果。
type ParsedStream struct {
	AudioData  []byte
	Chunks     int
	TextWords  int
	FirstChunk time.Duration // 从请求发起到收到第一个 sentence chunk 的耗时
	HasUsage   bool
	Subtitles  []dto.SubtitleEntry
}

// ParseStream 读取 v3 chunked NDJSON 响应,按文档 5.1 节的 event 取值分类处理。
//
// 关键修复(对比原实现):只有 event == "sentence" 才是音频帧;
// TTSSubtitle 单独收集,不会污染音频字节流。
func ParseStream(body io.Reader, started time.Time) (*ParsedStream, error) {
	out := &ParsedStream{}
	scanner := bufio.NewScanner(body)
	scanner.Buffer(make([]byte, 1024*1024), 8*1024*1024)

	gotFirstChunk := false
	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}
		var resp dto.V3TTSResponse
		if err := json.Unmarshal(line, &resp); err != nil {
			log.Printf("volcano: 解析响应行失败: %v, line=%q", err, truncateForLog(line, 200))
			continue
		}

		if resp.Code != 0 && resp.Code != 20000000 {
			return nil, &UpstreamError{
				Code:    resp.Code,
				Message: resp.Message,
				Stage:   "stream",
			}
		}

		if resp.Code == 20000000 {
			if resp.Usage != nil {
				out.TextWords = resp.Usage.TextWords
				out.HasUsage = true
				log.Printf("TTS 合成结束, usage: text_words=%d", out.TextWords)
			}
			for scanner.Scan() {
			}
			break
		}

		// 事件分发:显式匹配已知事件,绝不把未知事件当作音频。
		switch resp.Event {
		case "TTSSentenceStart":
			log.Printf("Sentence start: sequence=%d, sentence=%s", resp.Sequence, resp.Sentence)
		case "TTSSentenceEnd":
			log.Printf("Sentence end: sequence=%d", resp.Sequence)
		case "TTSSubtitle":
			if resp.Data != "" {
				out.Subtitles = append(out.Subtitles, dto.SubtitleEntry{
					Text:     resp.Sentence,
					Sequence: resp.Sequence,
				})
			}
		case "sentence":
			if resp.Data == "" {
				continue
			}
			chunk, err := base64.StdEncoding.DecodeString(resp.Data)
			if err != nil {
				return nil, &UpstreamError{
					Code:    resp.Code,
					Message: fmt.Sprintf("decode audio chunk: %v", err),
					Stage:   "stream",
					Wrapped: err,
				}
			}
			out.AudioData = append(out.AudioData, chunk...)
			out.Chunks++
			if !gotFirstChunk {
				out.FirstChunk = time.Since(started)
				gotFirstChunk = true
			}
		case "":
			// 传输帧,跳过
		default:
			log.Printf("volcano: 忽略未识别事件 event=%q sequence=%d", resp.Event, resp.Sequence)
		}
	}

	if err := scanner.Err(); err != nil {
		return nil, &UpstreamError{
			Code:    0,
			Message: fmt.Sprintf("read stream: %v", err),
			Stage:   "stream",
			Wrapped: err,
		}
	}

	if len(out.AudioData) == 0 {
		return nil, &UpstreamError{
			Code:    0,
			Message: "no audio data received from TTS service",
			Stage:   "stream",
		}
	}

	return out, nil
}

// ReadErrorBody 把非 200 响应的 body 读出来用于日志。
func ReadErrorBody(body io.Reader) string {
	const max = 2048
	buf := make([]byte, max)
	n, err := io.ReadFull(body, buf)
	if err != nil && !errors.Is(err, io.ErrUnexpectedEOF) && !errors.Is(err, io.EOF) {
		return fmt.Sprintf("read body fail: %v", err)
	}
	return string(buf[:n])
}

func truncateForLog(b []byte, max int) string {
	if len(b) > max {
		return string(b[:max]) + fmt.Sprintf("...(truncated, total %d bytes)", len(b))
	}
	return string(b)
}
