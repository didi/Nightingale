package sender

import (
	"errors"
	"time"

	"github.com/ccfos/nightingale/v6/alert/astats"
	"github.com/ccfos/nightingale/v6/models"
	"github.com/ccfos/nightingale/v6/pkg/ctx"
	"github.com/toolkits/pkg/container/list"
	"github.com/toolkits/pkg/logger"
)

// 通知记录队列，最大长度 1000000
var NotifyRecordQueue = list.NewSafeListLimited(1000000)

// 每秒上报通知记录队列大小
func ReportNotifyRecordQueueSize(stats *astats.Stats) {
	for {
		time.Sleep(time.Second)
		stats.GaugeNotifyRecordQueueSize.Set(float64(NotifyRecordQueue.Len()))
	}
}

// 推送通知记录到队列
// 若队列满 则返回 error
func PushNotifyRecords(records []*models.NotificaitonRecord) error {
	for _, record := range records {
		if ok := NotifyRecordQueue.PushFront(record); !ok {
			logger.Warningf("notify record queue is full, record: %+v", record)
			return errors.New("notify record queue is full")
		}
	}

	return nil
}

type NotifyRecordConsumer struct {
	ctx *ctx.Context
}

func NewNotifyRecordConsumer(ctx *ctx.Context) *NotifyRecordConsumer {
	return &NotifyRecordConsumer{
		ctx: ctx,
	}
}

// 消费通知记录队列 每 100ms 检测一次队列是否为空
func (c *NotifyRecordConsumer) LoopConsume() {
	duration := time.Duration(100) * time.Millisecond
	for {
		// 无论队列是否为空 都需要等待
		time.Sleep(duration)

		inotis := NotifyRecordQueue.PopBackBy(100)

		if len(inotis) == 0 {
			continue
		}

		// 类型转换，不然 CreateInBatches 会报错
		notis := make([]*models.NotificaitonRecord, 0, len(inotis))
		for _, inoti := range inotis {
			notis = append(notis, inoti.(*models.NotificaitonRecord))
		}

		c.consume(notis)
	}
}

func (c *NotifyRecordConsumer) consume(notis []*models.NotificaitonRecord) {
	if err := models.DB(c.ctx).CreateInBatches(notis, 100).Error; err != nil {
		logger.Errorf("add notis:%v failed, err: %v", notis, err)
	}
}
