package dispatch

// EventRetryComsumer 会尝试重新发送失败的事件
// 从 redis 中获取失败的事件
// 循环重新发送，直到发送成功

import (
	"encoding/json"
	"time"

	"github.com/ccfos/nightingale/v6/models"
	"github.com/ccfos/nightingale/v6/pkg/ctx"
	"github.com/ccfos/nightingale/v6/pkg/poster"
	"github.com/ccfos/nightingale/v6/storage"
	"github.com/toolkits/pkg/logger"
)

type EventRetryComsumer struct {
	ctx   *ctx.Context
	redis storage.Redis

	retryinterval  time.Duration // 重试间隔
	reportinterval time.Duration // 上报队列长度间隔

	dispatch *Dispatch

	maxlen int // 队列最大长度
	curlen int
}

func NewEventRetryComsumer(ctx *ctx.Context, redis storage.Redis, dp *Dispatch) *EventRetryComsumer {
	return &EventRetryComsumer{
		ctx:            ctx,
		redis:          redis,
		retryinterval:  5 * time.Second,
		reportinterval: 1 * time.Second,
		dispatch:       dp,
		maxlen:         100000,
	}
}

func (erc *EventRetryComsumer) PushEventToQueue(event *models.AlertCurEvent) {
	// 检查队列长度
	if erc.curlen >= erc.maxlen {
		logger.Errorf("failed to push event to queue: queue is full")
		return
	}

	// 序列化后推入 redis
	if data, err := json.Marshal(event); err == nil {
		if err := erc.redis.LPush(erc.ctx.Ctx, "failed_event_queue", data); err != nil {
			logger.Errorf("event:%+v push failed_event_queue err:%v", event, err)
		}
	} else {
		logger.Errorf("event:%+v marshal failed err:%v", event, err)
	}
}

func (erc *EventRetryComsumer) Start() {
	if erc.ctx.IsCenter {
		return
	}

	go erc.loopComsume()
	go erc.loopReport()
}

// 循环重发失败的事件
func (erc *EventRetryComsumer) loopComsume() {
	for {
		time.Sleep(100 * time.Millisecond)

		// 阻塞获取消息
		// result[0]: 队列名称
		// result[1]: 消息内容
		result, err := erc.redis.BLPop(erc.ctx.Ctx, 0, "failed_event_queue").Result()
		if err != nil {
			logger.Errorf("failed to pop from redis: %v", err)
			continue
		}

		// 检查消息格式
		if len(result) < 2 {
			logger.Errorf("failed to pop from redis: invalid result")
			continue
		}

		// 解析消息
		var event models.AlertCurEvent
		if err := json.Unmarshal([]byte(result[1]), &event); err != nil {
			// 解析失败，直接丢弃消息
			logger.Errorf("failed to unmarshal event: %v", err)
			continue
		}

		// 重新发送，直到成功
		for {
			var sendErr error
			if event.Id, sendErr = poster.PostByUrlsWithResp[int64](erc.ctx, "/v1/n9e/event-persist", &event); sendErr != nil {
				logger.Errorf("failed to send event: %v", sendErr)
				time.Sleep(erc.retryinterval)
				continue
			}

			break
		}
	}
}

// 循环上报队列长度
func (erc *EventRetryComsumer) loopReport() {
	for {
		time.Sleep(erc.reportinterval)

		// 汇报当前队列长度
		length, err := erc.redis.LLen(erc.ctx.Ctx, "failed_event_queue").Result()
		if err != nil {
			logger.Errorf("failed to get failed_event_queue length: %v", err)
			continue
		}

		erc.curlen = int(length)
		erc.dispatch.Astats.GuageEventRetryQueueSize.Set(float64(length))
	}
}
