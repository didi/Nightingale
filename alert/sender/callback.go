package sender

import (
	"html/template"
	"net/url"
	"strings"
	"time"

	"github.com/ccfos/nightingale/v6/alert/astats"
	"github.com/ccfos/nightingale/v6/memsto"
	"github.com/ccfos/nightingale/v6/models"
	"github.com/ccfos/nightingale/v6/pkg/ctx"
	"github.com/ccfos/nightingale/v6/pkg/poster"

	"github.com/toolkits/pkg/logger"
)

type (
	// CallBacker 进行回调的接口
	CallBacker interface {
		CallBack(ctx CallBackContext)
	}

	// CallBackContext 回调时所需的上下文
	CallBackContext struct {
		Ctx         *ctx.Context
		CallBackURL string
		Users       []*models.User
		Rule        *models.AlertRule
		Events      []*models.AlertCurEvent
		Stats       *astats.Stats
		BatchSend   bool
	}

	DefaultCallBacker struct{}
)

func BuildCallBackContext(ctx *ctx.Context, callBackURL string, rule *models.AlertRule, events []*models.AlertCurEvent,
	uids []int64, userCache *memsto.UserCacheType, batchSend bool, stats *astats.Stats) CallBackContext {
	users := userCache.GetByUserIds(uids)

	newCallBackUrl, _ := events[0].ParseURL(callBackURL)
	return CallBackContext{
		Ctx:         ctx,
		CallBackURL: newCallBackUrl,
		Rule:        rule,
		Events:      events,
		Users:       users,
		BatchSend:   batchSend,
		Stats:       stats,
	}
}

func ExtractAtsParams(rawURL string) []string {
	ans := make([]string, 0, 1)
	parsedURL, err := url.Parse(rawURL)
	if err != nil {
		logger.Errorf("ExtractAtsParams(url=%s), err: %v", rawURL, err)
		return ans
	}

	queryParams := parsedURL.Query()
	atParam := queryParams.Get("ats")
	if atParam == "" {
		return ans
	}

	// Split the atParam by comma and return the result as a slice
	return strings.Split(atParam, ",")
}

func NewCallBacker(
	key string,
	targetCache *memsto.TargetCacheType,
	userCache *memsto.UserCacheType,
	taskTplCache *memsto.TaskTplCache,
	tpls map[string]*template.Template,
) CallBacker {

	switch key {
	case models.IbexDomain: // Distribute to Ibex
		return &IbexCallBacker{
			targetCache:  targetCache,
			userCache:    userCache,
			taskTplCache: taskTplCache,
		}
	case models.DefaultDomain: // default callback
		return &DefaultCallBacker{}
	case models.DingtalkDomain:
		return &DingtalkSender{tpl: tpls[models.Dingtalk]}
	case models.WecomDomain:
		return &WecomSender{tpl: tpls[models.Wecom]}
	case models.FeishuDomain:
		return &FeishuSender{tpl: tpls[models.Feishu]}
	case models.FeishuCardDomain:
		return &FeishuCardSender{tpl: tpls[models.FeishuCard]}
	//case models.Mm:
	//	return &MmSender{tpl: tpls[models.Mm]}
	case models.TelegramDomain:
		return &TelegramSender{tpl: tpls[models.Telegram]}
	case models.LarkDomain:
		return &LarkSender{tpl: tpls[models.Lark]}
	case models.LarkCardDomain:
		return &LarkCardSender{tpl: tpls[models.LarkCard]}
	}

	return nil
}

func (c *DefaultCallBacker) CallBack(ctx CallBackContext) {
	if len(ctx.CallBackURL) == 0 || len(ctx.Events) == 0 {
		return
	}

	event := ctx.Events[0]

	if ctx.BatchSend {
		webhookConf := &models.Webhook{
			Type:          models.RuleCallback,
			Enable:        true,
			Url:           ctx.CallBackURL,
			Timeout:       5,
			RetryCount:    3,
			RetryInterval: 10,
			Batch:         1000,
		}

		PushCallbackEvent(ctx.Ctx, webhookConf, event, ctx.Stats)
		return
	}

	doSendAndRecord(ctx.Ctx, ctx.CallBackURL, ctx.CallBackURL, event, "callback", ctx.Stats, ctx.Events)
}

func doSendAndRecord(ctx *ctx.Context, url, token string, body interface{}, channel string,
	stats *astats.Stats, events []*models.AlertCurEvent) {
	res, err := doSend(url, body, channel, stats)
	NotifyRecord(ctx, events, channel, token, res, err)
}

func NotifyRecord(ctx *ctx.Context, evts []*models.AlertCurEvent, channel, target, res string, err error) {
	// 一个通知可能对应多个 event，都需要记录
	notis := make([]*models.NotificaitonRecord, 0, len(evts))
	for _, evt := range evts {
		noti := models.NewNotificationRecord(evt, channel, target)
		if err != nil {
			noti.SetStatus(models.NotiStatusFailure)
			noti.SetDetails(err.Error())
		} else if res != "" {
			noti.SetDetails(string(res))
		}
		notis = append(notis, noti)
	}

	if !ctx.IsCenter {
		err := poster.PostByUrls(ctx, "/v1/n9e/notify-record", notis)
		if err != nil {
			logger.Errorf("add notis:%v failed, err: %v", notis, err)
		}
		return
	}

	PushNotifyRecords(notis)
}

func doSend(url string, body interface{}, channel string, stats *astats.Stats) (string, error) {
	stats.AlertNotifyTotal.WithLabelValues(channel).Inc()

	res, code, err := poster.PostJSON(url, time.Second*5, body, 3)
	if err != nil {
		logger.Errorf("%s_sender: result=fail url=%s code=%d error=%v req:%v response=%s", channel, url, code, err, body, string(res))
		stats.AlertNotifyErrorTotal.WithLabelValues(channel).Inc()
		return "", err
	}

	logger.Infof("%s_sender: result=succ url=%s code=%d req:%v response=%s", channel, url, code, body, string(res))
	return string(res), nil
}

type TaskCreateReply struct {
	Err string `json:"err"`
	Dat int64  `json:"dat"` // task.id
}

func PushCallbackEvent(ctx *ctx.Context, webhook *models.Webhook, event *models.AlertCurEvent, stats *astats.Stats) {
	CallbackEventQueueLock.RLock()
	queue := CallbackEventQueue[webhook.Url]
	CallbackEventQueueLock.RUnlock()

	if queue == nil {
		queue = &WebhookQueue{
			eventQueue: NewSafeEventQueue(QueueMaxSize),
			closeCh:    make(chan struct{}),
		}

		CallbackEventQueueLock.Lock()
		CallbackEventQueue[webhook.Url] = queue
		CallbackEventQueueLock.Unlock()

		StartConsumer(ctx, queue, webhook.Batch, webhook, stats)
	}

	succ := queue.eventQueue.Push(event)
	if !succ {
		logger.Warningf("Write channel(%s) full, current channel size: %d event:%v", webhook.Url, queue.eventQueue.Len(), event)
	}
}
