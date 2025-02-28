package dispatch

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/ccfos/nightingale/v6/alert/aconf"
	"github.com/ccfos/nightingale/v6/alert/common"
	"github.com/ccfos/nightingale/v6/alert/queue"
	"github.com/ccfos/nightingale/v6/models"
	"github.com/ccfos/nightingale/v6/pkg/ctx"
	"github.com/ccfos/nightingale/v6/pkg/poster"
	promsdk "github.com/ccfos/nightingale/v6/pkg/prom"
	"github.com/ccfos/nightingale/v6/pkg/tplx"
	"github.com/ccfos/nightingale/v6/prom"

	"github.com/prometheus/common/model"
	"github.com/toolkits/pkg/concurrent/semaphore"
	"github.com/toolkits/pkg/logger"
)

type Consumer struct {
	alerting aconf.Alerting
	ctx      *ctx.Context

	dispatch    *Dispatch
	promClients *prom.PromClientMap

	eventRetryConsumer *EventRetryComsumer
}

func InitRegisterQueryFunc(promClients *prom.PromClientMap) {
	tplx.RegisterQueryFunc(func(datasourceID int64, promql string) model.Value {
		if promClients.IsNil(datasourceID) {
			return nil
		}

		readerClient := promClients.GetCli(datasourceID)
		value, _, _ := readerClient.Query(context.Background(), promql, time.Now())
		return value
	})
}

// 创建一个 Consumer 实例
func NewConsumer(alerting aconf.Alerting, ctx *ctx.Context, dispatch *Dispatch, promClients *prom.PromClientMap, erc *EventRetryComsumer) *Consumer {
	return &Consumer{
		alerting:    alerting,
		ctx:         ctx,
		dispatch:    dispatch,
		promClients: promClients,

		eventRetryConsumer: erc,
	}
}

func (e *Consumer) LoopConsume() {
	sema := semaphore.NewSemaphore(e.alerting.NotifyConcurrency)
	duration := time.Duration(100) * time.Millisecond
	for {
		events := queue.EventQueue.PopBackBy(100)
		if len(events) == 0 {
			time.Sleep(duration)
			continue
		}
		e.consume(events, sema)
	}
}

func (e *Consumer) consume(events []interface{}, sema *semaphore.Semaphore) {
	for i := range events {
		if events[i] == nil {
			continue
		}

		event := events[i].(*models.AlertCurEvent)
		sema.Acquire()
		go func(event *models.AlertCurEvent) {
			defer sema.Release()
			e.consumeOne(event)
		}(event)
	}
}

func (e *Consumer) consumeOne(event *models.AlertCurEvent) {
	LogEvent(event, "consume")

	eventType := "alert"
	if event.IsRecovered {
		eventType = "recovery"
	}

	e.dispatch.Astats.CounterAlertsTotal.WithLabelValues(event.Cluster, eventType, event.GroupName).Inc()

	if err := event.ParseRule("rule_name"); err != nil {
		logger.Warningf("ruleid:%d failed to parse rule name: %v", event.RuleId, err)
		event.RuleName = fmt.Sprintf("failed to parse rule name: %v", err)
	}

	if err := event.ParseRule("annotations"); err != nil {
		logger.Warningf("ruleid:%d failed to parse annotations: %v", event.RuleId, err)
		event.Annotations = fmt.Sprintf("failed to parse annotations: %v", err)
		event.AnnotationsJSON["error"] = event.Annotations
	}

	e.queryRecoveryVal(event)

	if err := event.ParseRule("rule_note"); err != nil {
		logger.Warningf("ruleid:%d failed to parse rule note: %v", event.RuleId, err)
		event.RuleNote = fmt.Sprintf("failed to parse rule note: %v", err)
	}

	e.persist(event)

	if event.IsRecovered && event.NotifyRecovered == 0 {
		return
	}

	e.dispatch.HandleEventNotify(event, false)
}

func (e *Consumer) persist(event *models.AlertCurEvent) {
	if event.Status != 0 {
		return
	}

	if !e.ctx.IsCenter {
		event.DB2FE()
		var err error
		event.Id, err = poster.PostByUrlsWithResp[int64](e.ctx, "/v1/n9e/event-persist", event)
		if err != nil {
			logger.Errorf("event:%+v persist err:%v", event, err)

			// 将失败的事件放入重试队列
			e.eventRetryConsumer.PushEventToQueue(event)

			e.dispatch.Astats.CounterRuleEvalErrorTotal.WithLabelValues(fmt.Sprintf("%v", event.DatasourceId), "persist_event", event.GroupName, fmt.Sprintf("%v", event.RuleId)).Inc()
		}
		return
	}

	err := models.EventPersist(e.ctx, event)
	if err != nil {
		logger.Errorf("event%+v persist err:%v", event, err)
		e.dispatch.Astats.CounterRuleEvalErrorTotal.WithLabelValues(fmt.Sprintf("%v", event.DatasourceId), "persist_event", event.GroupName, fmt.Sprintf("%v", event.RuleId)).Inc()
	}
}

func (e *Consumer) queryRecoveryVal(event *models.AlertCurEvent) {
	if !event.IsRecovered {
		return
	}

	// If the event is a recovery event, execute the recovery_promql query
	promql, ok := event.AnnotationsJSON["recovery_promql"]
	if !ok {
		return
	}

	promql = strings.TrimSpace(promql)
	if promql == "" {
		logger.Warningf("rule_eval:%s promql is blank", getKey(event))
		return
	}

	if e.promClients.IsNil(event.DatasourceId) {
		logger.Warningf("rule_eval:%s error reader client is nil", getKey(event))
		return
	}

	readerClient := e.promClients.GetCli(event.DatasourceId)

	var warnings promsdk.Warnings
	value, warnings, err := readerClient.Query(e.ctx.Ctx, promql, time.Now())
	if err != nil {
		logger.Errorf("rule_eval:%s promql:%s, error:%v", getKey(event), promql, err)
		event.AnnotationsJSON["recovery_promql_error"] = fmt.Sprintf("promql:%s error:%v", promql, err)

		b, err := json.Marshal(event.AnnotationsJSON)
		if err != nil {
			event.AnnotationsJSON = make(map[string]string)
			event.AnnotationsJSON["error"] = fmt.Sprintf("failed to parse annotations: %v", err)
		} else {
			event.Annotations = string(b)
		}
		return
	}

	if len(warnings) > 0 {
		logger.Errorf("rule_eval:%s promql:%s, warnings:%v", getKey(event), promql, warnings)
	}

	anomalyPoints := models.ConvertAnomalyPoints(value)
	if len(anomalyPoints) == 0 {
		logger.Warningf("rule_eval:%s promql:%s, result is empty", getKey(event), promql)
		event.AnnotationsJSON["recovery_promql_error"] = fmt.Sprintf("promql:%s error:%s", promql, "result is empty")
	} else {
		event.AnnotationsJSON["recovery_value"] = fmt.Sprintf("%v", anomalyPoints[0].Value)
	}

	b, err := json.Marshal(event.AnnotationsJSON)
	if err != nil {
		event.AnnotationsJSON = make(map[string]string)
		event.AnnotationsJSON["error"] = fmt.Sprintf("failed to parse annotations: %v", err)
	} else {
		event.Annotations = string(b)
	}
}

func getKey(event *models.AlertCurEvent) string {
	return common.RuleKey(event.DatasourceId, event.RuleId)
}
