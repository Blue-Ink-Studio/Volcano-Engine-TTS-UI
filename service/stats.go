package service

import (
	"fmt"
	"runtime"
	"sync"
	"time"

	"github.com/volcano-tts/tts-api/common"
)

type Stats struct {
	totalRequests       int64
	successfulRequests  int64
	failedRequests      int64
	totalResponseTime   time.Duration
	recentResponseTimes []float64
	responseTimesIndex  int
	lastErrors          []string
	errorsIndex         int
	mutex               sync.RWMutex
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

	if success {
		s.successfulRequests++
	} else {
		s.failedRequests++
		if errMsg != "" {
			errInfo := fmt.Sprintf("%s: %s", time.Now().Format(time.RFC3339), errMsg)
			s.lastErrors[s.errorsIndex] = errInfo
			s.errorsIndex = (s.errorsIndex + 1) % common.MaxErrors
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

	recentResponseTimes = make([]float64, 0, common.MaxResponseTimes)
	for _, t := range s.recentResponseTimes {
		if t > 0 {
			recentResponseTimes = append(recentResponseTimes, t)
		}
	}

	lastErrors = make([]string, 0, common.MaxErrors)
	for _, e := range s.lastErrors {
		if e != "" {
			lastErrors = append(lastErrors, e)
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
