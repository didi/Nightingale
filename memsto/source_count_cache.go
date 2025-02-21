package memsto

// SourceCountCache 用于缓存 source ip 在一定时间内的上报次数

import (
	"sync"
)

type SourceCountCache struct {
	sourceMetricsStats map[string]int
	mu                 sync.Mutex
}

func NewSourceCountCache() *SourceCountCache {
	sc := &SourceCountCache{
		sourceMetricsStats: make(map[string]int),
	}

	return sc
}

// 将 source ip 记录到 SourceMetricsStats 中，若存在则数量加一
func (s *SourceCountCache) Increase(source string) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.sourceMetricsStats[source]++
}

// 获取缓存 map 并清空
func (s *SourceCountCache) GetAndFlush() map[string]int {
	s.mu.Lock()
	defer s.mu.Unlock()

	currentStats := s.sourceMetricsStats
	s.sourceMetricsStats = make(map[string]int)

	return currentStats
}
