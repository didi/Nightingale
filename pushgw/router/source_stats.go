package router

// source_state.go 用于上报超过采样阈值的 source ip 统计数据
// 同时清理不再更新，或恢复正常的 source ip 统计数据

import (
	"math"
	"sync"
	"time"

	"github.com/ccfos/nightingale/v6/memsto"
)

var (
	SourceStats                   *memsto.SourceCountCache
	loopReportSourceStatsInterval = time.Second * 60
	reportcache                   = sync.Map{}
)

type reportItemEntry struct {
	lastReportTs int64
	lastUpdateTs int64
}

// 更新上报时间
func (re *reportItemEntry) Report(ts int64) {
	re.lastReportTs = ts
}

// 更新最后更新时间
func (re *reportItemEntry) Update(ts int64) {
	re.lastUpdateTs = ts
}

func init() {
	SourceStats = memsto.NewSourceCountCache()
}

// 启动上报 source ip 统计数据
func (r *Router) ReportSourceStats() {
	if !r.Pushgw.EnableSourceStats {
		return
	}

	go r.loopReportSourceStats()
	go r.loopCleanSourceStats()
}

// 上报 source ip 统计数据
func (r *Router) loopReportSourceStats() {
	for {
		time.Sleep(loopReportSourceStatsInterval)

		currentStats := SourceStats.GetAndFlush()
		now := time.Now().Unix()

		for source, count := range currentStats {
			overThreshold := false
			if count >= r.Pushgw.SourceStatsThreshold {
				CounterSampleReceivedBySource.WithLabelValues(source).Set(float64(count))
				overThreshold = true
			}

			// 更新提交时间与更新时间
			var re *reportItemEntry
			if item, ok := reportcache.Load(source); ok {
				// 本身就在上报缓存中 更新时间
				re = item.(*reportItemEntry)

				re.Update(now)
				if overThreshold {
					re.Report(now)
				} else {
					// 虽然没到阈值，但是之前上报过，仍需要上报一段时间
					CounterSampleReceivedBySource.WithLabelValues(source).Set(float64(count))
				}

			} else if overThreshold {
				// 不在上报缓存中，且需要上报，添加到上报缓存
				re = &reportItemEntry{
					lastReportTs: now,
					lastUpdateTs: now,
				}

				reportcache.Store(source, re)
			}
		}

		// 立刻删除不再更新的 source
		reportcache.Range(func(key, value interface{}) bool {
			source := key.(string)
			re := value.(*reportItemEntry)
			if now-re.lastUpdateTs > int64(loopReportSourceStatsInterval.Seconds()) {
				CounterSampleReceivedBySource.WithLabelValues(source).Set(math.NaN())
				reportcache.Delete(source)
			}
			return true
		})
	}
}

// 循环检测，若超过一定时间超过阈值则删除
func (r *Router) loopCleanSourceStats() {
	looptime := time.Minute * 5
	cleantime := time.Minute * 10

	for {
		time.Sleep(looptime)
		now := time.Now().Unix()

		reportcache.Range(func(key, value interface{}) bool {
			source := key.(string)
			re := value.(*reportItemEntry)
			if now-re.lastReportTs > int64(cleantime.Seconds())+int64(loopReportSourceStatsInterval.Seconds()) {
				CounterSampleReceivedBySource.WithLabelValues(source).Set(math.NaN())
				reportcache.Delete(source)
			}
			return true
		})
	}
}
