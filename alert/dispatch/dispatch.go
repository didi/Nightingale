package dispatch

import (
	"bytes"
	"encoding/json"
	"html/template"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/ccfos/nightingale/v6/alert/aconf"
	"github.com/ccfos/nightingale/v6/alert/astats"
	"github.com/ccfos/nightingale/v6/alert/common"
	"github.com/ccfos/nightingale/v6/alert/sender"
	"github.com/ccfos/nightingale/v6/memsto"
	"github.com/ccfos/nightingale/v6/models"
	"github.com/ccfos/nightingale/v6/pkg/ctx"

	"github.com/toolkits/pkg/logger"
)

type Dispatch struct {
	alertRuleCache      *memsto.AlertRuleCacheType
	userCache           *memsto.UserCacheType
	userGroupCache      *memsto.UserGroupCacheType
	alertSubscribeCache *memsto.AlertSubscribeCacheType
	targetCache         *memsto.TargetCacheType
	notifyConfigCache   *memsto.NotifyConfigCacheType
	taskTplsCache       *memsto.TaskTplCache

	alerting aconf.Alerting

	Senders          map[string]sender.Sender
	CallBacks        map[string]sender.CallBacker
	tpls             map[string]*template.Template
	ExtraSenders     map[string]sender.Sender
	BeforeSenderHook func(*models.AlertCurEvent) bool

	ctx    *ctx.Context
	Astats *astats.Stats

	RwLock sync.RWMutex
}

// 创建一个 Notify 实例
func NewDispatch(alertRuleCache *memsto.AlertRuleCacheType, userCache *memsto.UserCacheType, userGroupCache *memsto.UserGroupCacheType,
	alertSubscribeCache *memsto.AlertSubscribeCacheType, targetCache *memsto.TargetCacheType, notifyConfigCache *memsto.NotifyConfigCacheType,
	taskTplsCache *memsto.TaskTplCache, alerting aconf.Alerting, ctx *ctx.Context, astats *astats.Stats) *Dispatch {
	notify := &Dispatch{
		alertRuleCache:      alertRuleCache,
		userCache:           userCache,
		userGroupCache:      userGroupCache,
		alertSubscribeCache: alertSubscribeCache,
		targetCache:         targetCache,
		notifyConfigCache:   notifyConfigCache,
		taskTplsCache:       taskTplsCache,

		alerting: alerting,

		Senders:          make(map[string]sender.Sender),
		tpls:             make(map[string]*template.Template),
		ExtraSenders:     make(map[string]sender.Sender),
		BeforeSenderHook: func(*models.AlertCurEvent) bool { return true },

		ctx:    ctx,
		Astats: astats,
	}
	return notify
}

func (e *Dispatch) ReloadTpls() error {
	err := e.relaodTpls()
	if err != nil {
		logger.Errorf("failed to reload tpls: %v", err)
	}

	duration := time.Duration(9000) * time.Millisecond
	for {
		time.Sleep(duration)
		if err := e.relaodTpls(); err != nil {
			logger.Warning("failed to reload tpls:", err)
		}
	}
}

func (e *Dispatch) relaodTpls() error {
	tmpTpls, err := models.ListTpls(e.ctx)
	if err != nil {
		return err
	}
	smtp := e.notifyConfigCache.GetSMTP()

	senders := map[string]sender.Sender{
		models.Email:      sender.NewSender(models.Email, tmpTpls, smtp),
		models.Dingtalk:   sender.NewSender(models.Dingtalk, tmpTpls),
		models.Wecom:      sender.NewSender(models.Wecom, tmpTpls),
		models.Feishu:     sender.NewSender(models.Feishu, tmpTpls),
		models.Mm:         sender.NewSender(models.Mm, tmpTpls),
		models.Telegram:   sender.NewSender(models.Telegram, tmpTpls),
		models.FeishuCard: sender.NewSender(models.FeishuCard, tmpTpls),
		models.Lark:       sender.NewSender(models.Lark, tmpTpls),
		models.LarkCard:   sender.NewSender(models.LarkCard, tmpTpls),
	}

	// domain -> Callback()
	callbacks := map[string]sender.CallBacker{
		models.DingtalkDomain:   sender.NewCallBacker(models.DingtalkDomain, e.targetCache, e.userCache, e.taskTplsCache, tmpTpls),
		models.WecomDomain:      sender.NewCallBacker(models.WecomDomain, e.targetCache, e.userCache, e.taskTplsCache, tmpTpls),
		models.FeishuDomain:     sender.NewCallBacker(models.FeishuDomain, e.targetCache, e.userCache, e.taskTplsCache, tmpTpls),
		models.TelegramDomain:   sender.NewCallBacker(models.TelegramDomain, e.targetCache, e.userCache, e.taskTplsCache, tmpTpls),
		models.FeishuCardDomain: sender.NewCallBacker(models.FeishuCardDomain, e.targetCache, e.userCache, e.taskTplsCache, tmpTpls),
		models.IbexDomain:       sender.NewCallBacker(models.IbexDomain, e.targetCache, e.userCache, e.taskTplsCache, tmpTpls),
		models.LarkDomain:       sender.NewCallBacker(models.LarkDomain, e.targetCache, e.userCache, e.taskTplsCache, tmpTpls),
		models.DefaultDomain:    sender.NewCallBacker(models.DefaultDomain, e.targetCache, e.userCache, e.taskTplsCache, tmpTpls),
		models.LarkCardDomain:   sender.NewCallBacker(models.LarkCardDomain, e.targetCache, e.userCache, e.taskTplsCache, tmpTpls),
	}

	e.RwLock.RLock()
	for channelName, extraSender := range e.ExtraSenders {
		senders[channelName] = extraSender
	}
	e.RwLock.RUnlock()

	e.RwLock.Lock()
	e.tpls = tmpTpls
	e.Senders = senders
	e.CallBacks = callbacks
	e.RwLock.Unlock()
	return nil
}

// HandleEventNotify 处理event事件的主逻辑
// event: 告警/恢复事件
// isSubscribe: 告警事件是否由subscribe的配置产生
func (e *Dispatch) HandleEventNotify(event *models.AlertCurEvent, isSubscribe bool) {
	rule := e.alertRuleCache.Get(event.RuleId)
	if rule == nil {
		return
	}

	if e.blockEventNotify(rule, event) {
		logger.Infof("block event notify: rule_id:%d event:%+v", rule.Id, event)
		return
	}

	fillUsers(event, e.userCache, e.userGroupCache)

	var (
		// 处理事件到 notifyTarget 关系,处理的notifyTarget用OrMerge进行合并
		handlers []NotifyTargetDispatch

		// 额外去掉一些订阅,处理的notifyTarget用AndMerge进行合并, 如设置 channel=false,合并后不通过这个channel发送
		// 如果实现了相关 Dispatch,可以添加到interceptors中
		interceptorHandlers []NotifyTargetDispatch
	)
	if isSubscribe {
		handlers = []NotifyTargetDispatch{NotifyGroupDispatch, EventCallbacksDispatch}
	} else {
		handlers = []NotifyTargetDispatch{NotifyGroupDispatch, GlobalWebhookDispatch, EventCallbacksDispatch}
	}

	notifyTarget := NewNotifyTarget()
	// 处理订阅关系使用OrMerge
	for _, handler := range handlers {
		notifyTarget.OrMerge(handler(rule, event, notifyTarget, e))
	}

	// 处理移除订阅关系的逻辑,比如员工离职，临时静默某个通道的策略等
	for _, handler := range interceptorHandlers {
		notifyTarget.AndMerge(handler(rule, event, notifyTarget, e))
	}

	// 处理事件发送,这里用一个goroutine处理一个event的所有发送事件
	go e.Send(rule, event, notifyTarget, isSubscribe)

	// 如果是不是订阅规则出现的event, 则需要处理订阅规则的event
	if !isSubscribe {
		e.handleSubs(event)
	}
}

func (e *Dispatch) blockEventNotify(rule *models.AlertRule, event *models.AlertCurEvent) bool {
	ruleType := rule.GetRuleType()

	// 若为机器则先看机器是否删除
	if ruleType == models.HOST {
		host, ok := e.targetCache.Get(event.TagsMap["ident"])
		if !ok || host == nil {
			return true
		}
	}

	// 恢复通知，检测规则配置是否改变
	// if event.IsRecovered && event.RuleHash != rule.Hash() {
	// 	return true
	// }

	return false
}

func (e *Dispatch) handleSubs(event *models.AlertCurEvent) {
	// handle alert subscribes
	subscribes := make([]*models.AlertSubscribe, 0)
	// rule specific subscribes
	if subs, has := e.alertSubscribeCache.Get(event.RuleId); has {
		subscribes = append(subscribes, subs...)
	}
	// global subscribes
	if subs, has := e.alertSubscribeCache.Get(0); has {
		subscribes = append(subscribes, subs...)
	}

	for _, sub := range subscribes {
		e.handleSub(sub, *event)
	}
}

// handleSub 处理订阅规则的event,注意这里event要使用值传递,因为后面会修改event的状态
func (e *Dispatch) handleSub(sub *models.AlertSubscribe, event models.AlertCurEvent) {
	if sub.IsDisabled() {
		return
	}

	if !sub.MatchCluster(event.DatasourceId) {
		return
	}

	if !sub.MatchProd(event.RuleProd) {
		return
	}

	if !common.MatchTags(event.TagsMap, sub.ITags) {
		return
	}
	// event BusiGroups filter
	if !common.MatchGroupsName(event.GroupName, sub.IBusiGroups) {
		return
	}
	if sub.ForDuration > (event.TriggerTime - event.FirstTriggerTime) {
		return
	}

	if len(sub.SeveritiesJson) != 0 {
		match := false
		for _, s := range sub.SeveritiesJson {
			if s == event.Severity || s == 0 {
				match = true
				break
			}
		}
		if !match {
			return
		}
	}

	e.Astats.CounterSubEventTotal.WithLabelValues(event.GroupName).Inc()
	sub.ModifyEvent(&event)
	event.SubRuleId = sub.Id

	LogEvent(&event, "subscribe")
	e.HandleEventNotify(&event, true)
}

func (e *Dispatch) Send(rule *models.AlertRule, event *models.AlertCurEvent, notifyTarget *NotifyTarget, isSubscribe bool) {
	needSend := e.BeforeSenderHook(event)
	if needSend {
		for channel, uids := range notifyTarget.ToChannelUserMap() {
			msgCtx := sender.BuildMessageContext(e.ctx, rule, []*models.AlertCurEvent{event},
				uids, e.userCache, e.Astats)
			e.RwLock.RLock()
			s := e.Senders[channel]
			e.RwLock.RUnlock()
			if s == nil {
				logger.Debugf("no sender for channel: %s", channel)
				continue
			}

			var event *models.AlertCurEvent
			if len(msgCtx.Events) > 0 {
				event = msgCtx.Events[0]
			}

			logger.Debugf("send to channel:%s event:%+v users:%+v", channel, event, msgCtx.Users)
			s.Send(msgCtx)
		}
	}

	// handle event callbacks
	e.SendCallbacks(rule, notifyTarget, event)

	// handle global webhooks
	if !event.OverrideGlobalWebhook() {
		if e.alerting.WebhookBatchSend {
			sender.BatchSendWebhooks(e.ctx, notifyTarget.ToWebhookMap(), event, e.Astats)
		} else {
			sender.SingleSendWebhooks(e.ctx, notifyTarget.ToWebhookMap(), event, e.Astats)
		}
	}

	// handle plugin call
	go sender.MayPluginNotify(e.ctx, e.genNoticeBytes(event), e.notifyConfigCache.
		GetNotifyScript(), e.Astats, event)

	if !isSubscribe {
		// handle ibex callbacks
		e.HandleIbex(rule, event)
	}
}

func (e *Dispatch) SendCallbacks(rule *models.AlertRule, notifyTarget *NotifyTarget, event *models.AlertCurEvent) {
	uids := notifyTarget.ToUidList()
	urls := notifyTarget.ToCallbackList()
	whMap := notifyTarget.ToWebhookMap()
	ogw := event.OverrideGlobalWebhook()
	for _, urlStr := range urls {
		if len(urlStr) == 0 {
			continue
		}

		cbCtx := sender.BuildCallBackContext(e.ctx, urlStr, rule, []*models.AlertCurEvent{event}, uids, e.userCache, e.alerting.WebhookBatchSend, e.Astats)

		if wh, ok := whMap[cbCtx.CallBackURL]; !ogw && ok && wh.Enable {
			logger.Debugf("SendCallbacks: webhook[%s] is in global conf.", cbCtx.CallBackURL)
			continue
		}

		if strings.HasPrefix(urlStr, "${ibex}") {
			e.CallBacks[models.IbexDomain].CallBack(cbCtx)
			continue
		}

		if !(strings.HasPrefix(urlStr, "http://") || strings.HasPrefix(urlStr, "https://")) {
			cbCtx.CallBackURL = "http://" + urlStr
		}

		parsedURL, err := url.Parse(urlStr)
		if err != nil {
			logger.Errorf("SendCallbacks: failed to url.Parse(urlStr=%s): %v", urlStr, err)
			continue
		}

		// process feishu card
		if parsedURL.Host == models.FeishuDomain && parsedURL.Query().Get("card") == "1" {
			e.CallBacks[models.FeishuCardDomain].CallBack(cbCtx)
			continue
		}

		// process lark card
		if parsedURL.Host == models.LarkDomain && parsedURL.Query().Get("card") == "1" {
			e.CallBacks[models.LarkCardDomain].CallBack(cbCtx)
			continue
		}

		callBacker, ok := e.CallBacks[parsedURL.Host]
		if ok {
			callBacker.CallBack(cbCtx)
		} else {
			e.CallBacks[models.DefaultDomain].CallBack(cbCtx)
		}
	}
}

func (e *Dispatch) HandleIbex(rule *models.AlertRule, event *models.AlertCurEvent) {
	// 解析 RuleConfig 字段
	var ruleConfig struct {
		TaskTpls []*models.Tpl `json:"task_tpls"`
	}
	json.Unmarshal([]byte(rule.RuleConfig), &ruleConfig)

	for _, t := range ruleConfig.TaskTpls {
		if t.TplId == 0 {
			continue
		}

		if len(t.Host) == 0 {
			sender.CallIbex(e.ctx, t.TplId, event.TargetIdent,
				e.taskTplsCache, e.targetCache, e.userCache, event)
			continue
		}
		for _, host := range t.Host {
			sender.CallIbex(e.ctx, t.TplId, host,
				e.taskTplsCache, e.targetCache, e.userCache, event)
		}
	}
}

type Notice struct {
	Event *models.AlertCurEvent `json:"event"`
	Tpls  map[string]string     `json:"tpls"`
}

func (e *Dispatch) genNoticeBytes(event *models.AlertCurEvent) []byte {
	// build notice body with templates
	ntpls := make(map[string]string)

	e.RwLock.RLock()
	defer e.RwLock.RUnlock()
	for filename, tpl := range e.tpls {
		var body bytes.Buffer
		if err := tpl.Execute(&body, event); err != nil {
			ntpls[filename] = err.Error()
		} else {
			ntpls[filename] = body.String()
		}
	}

	notice := Notice{Event: event, Tpls: ntpls}
	stdinBytes, err := json.Marshal(notice)
	if err != nil {
		logger.Errorf("event_notify: failed to marshal notice: %v", err)
		return nil
	}

	return stdinBytes
}

// for alerting
func fillUsers(ce *models.AlertCurEvent, uc *memsto.UserCacheType, ugc *memsto.UserGroupCacheType) {
	gids := make([]int64, 0, len(ce.NotifyGroupsJSON))
	for i := 0; i < len(ce.NotifyGroupsJSON); i++ {
		gid, err := strconv.ParseInt(ce.NotifyGroupsJSON[i], 10, 64)
		if err != nil {
			continue
		}
		gids = append(gids, gid)
	}

	ce.NotifyGroupsObj = ugc.GetByUserGroupIds(gids)

	uids := make(map[int64]struct{})
	for i := 0; i < len(ce.NotifyGroupsObj); i++ {
		ug := ce.NotifyGroupsObj[i]
		for j := 0; j < len(ug.UserIds); j++ {
			uids[ug.UserIds[j]] = struct{}{}
		}
	}

	ce.NotifyUsersObj = uc.GetByUserIds(mapKeys(uids))
}

func mapKeys(m map[int64]struct{}) []int64 {
	lst := make([]int64, 0, len(m))
	for k := range m {
		lst = append(lst, k)
	}
	return lst
}
