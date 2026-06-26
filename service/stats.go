package service

import (
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/volcano-tts/tts-api/common"
)

type Stats struct {
	totalRequests        int64
	successfulRequests   int64
	failedRequests       int64
	totalResponseTime    time.Duration
	recentResponseTimes  []float64
	responseTimesIndex   int
	responseTimesCount   int
	lastErrors           []string
	errorsIndex          int
	errorsCount          int
	mutex                sync.RWMutex
}

var GlobalStats *Stats

func InitStats() {
	GlobalStats = &Stats{
		recentResponseTimes: make([]float64, common.MaxResponseTimes),
		lastErrors:          make([]string, common.MaxErrors),
	}
}

func (s *Stats) AddRequest(success bool, responseTime time.Duration, errMsg string) {
	s.mutex.Lock()
	defer s.mutex.Unlock()

	s.totalRequests++
	s.totalResponseTime += responseTime

	s.recentResponseTimes[s.responseTimesIndex] = responseTime.Seconds() * 1000
	s.responseTimesIndex = (s.responseTimesIndex + 1) % common.MaxResponseTimes
	if s.responseTimesCount < common.MaxResponseTimes {
		s.responseTimesCount++
	}

	if success {
		s.successfulRequests++
	} else {
		s.failedRequests++
		if errMsg != "" {
			now := time.Now().Format(time.RFC3339)

			// 去重：如果最近一条错误的消息内容相同，仅更新时间戳
			if s.errorsCount > 0 {
				lastIdx := (s.errorsIndex - 1 + common.MaxErrors) % common.MaxErrors
				lastEntry := s.lastErrors[lastIdx]
				if sepIdx := strings.Index(lastEntry, ": "); sepIdx != -1 {
					if lastEntry[sepIdx+2:] == errMsg {
						s.lastErrors[lastIdx] = now + ": " + errMsg
						return
					}
				}
			}

			s.lastErrors[s.errorsIndex] = now + ": " + errMsg
			s.errorsIndex = (s.errorsIndex + 1) % common.MaxErrors
			if s.errorsCount < common.MaxErrors {
				s.errorsCount++
			}
		}
	}
}

func (s *Stats) GetSnapshot() (totalRequests int64, successfulRequests int64, failedRequests int64,
	totalResponseTime time.Duration, recentResponseTimes []float64, lastErrors []string) {
	s.mutex.RLock()
	defer s.mutex.RUnlock()

	totalRequests = s.totalRequests
	successfulRequests = s.successfulRequests
	failedRequests = s.failedRequests
	totalResponseTime = s.totalResponseTime

	// 按时间顺序（从旧到新）遍历响应时间环形缓冲区
	recentResponseTimes = make([]float64, 0, s.responseTimesCount)
	if s.responseTimesCount > 0 {
		start := 0
		if s.responseTimesCount == common.MaxResponseTimes {
			start = s.responseTimesIndex
		}
		for i := 0; i < s.responseTimesCount; i++ {
			idx := (start + i) % common.MaxResponseTimes
			recentResponseTimes = append(recentResponseTimes, s.recentResponseTimes[idx])
		}
	}

	// 按时间顺序（从旧到新）遍历错误环形缓冲区
	lastErrors = make([]string, 0, s.errorsCount)
	if s.errorsCount > 0 {
		start := 0
		if s.errorsCount == common.MaxErrors {
			start = s.errorsIndex
		}
		for i := 0; i < s.errorsCount; i++ {
			idx := (start + i) % common.MaxErrors
			lastErrors = append(lastErrors, s.lastErrors[idx])
		}
	}

	return
}

func GetMemoryInfo() map[string]interface{} {
	var m runtime.MemStats
	runtime.ReadMemStats(&m)
	return map[string]interface{}{
		"total_alloc": m.TotalAlloc,
		"heap_alloc":  m.HeapAlloc,
		"heap_inuse":  m.HeapInuse,
		"goroutines":  runtime.NumGoroutine(),
	}
}
