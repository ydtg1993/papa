package engine

import (
	"context"
	"fmt"
	"github.com/sirupsen/logrus"
	"github.com/ydtg1993/papa/v4/pkg/events"
	"strconv"
	"time"
)

// AddNotifier 注册告警通知器；任务最终失败时触发 Notify。
func (e *Engine) AddNotifier(n Notifier) {
	if n == nil {
		return
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	e.notifiers = append(e.notifiers, n)
}

func (e *Engine) getNotifiers() []Notifier {
	e.mu.RLock()
	defer e.mu.RUnlock()
	out := make([]Notifier, len(e.notifiers))
	copy(out, e.notifiers)
	return out
}

// notifyFailure 结构化记录失败日志，并触发告警通知器。
func (e *Engine) notifyFailure(ctx context.Context, task *Task, err error) {
	te := TaskError{
		Stage:   task.Stage,
		Site:    task.Site,
		TaskID:  task.ID,
		URL:     task.URL,
		Retry:   task.Retry,
		Kind:    ErrorKind(err),
		Message: err.Error(),
	}
	e.loggerSet.Engine.WithFields(logrus.Fields{
		"stage":   te.Stage,
		"site":    te.Site,
		"task_id": te.TaskID,
		"url":     te.URL,
		"retry":   te.Retry,
		"kind":    te.Kind,
	}).Errorf("task failed: %s", te.Message)

	// One persisted task round is observed by the sole detector. Attempts do not
	// increment the terminal count, and notifications use the frozen lifecycle ID.
	if st, tripped := e.recordBreakerFailure(task, err); tripped {
		// 通知在锁外发送，使用触发瞬间的快照，恢复不能擦掉告警现场。
		e.notifyBreakerTrip(st)
	}

	notifiers := e.getNotifiers()
	if len(notifiers) == 0 {
		return
	}
	level := AlertError
	if Retryable(err) {
		level = AlertWarn
	}
	event := AlertEvent{Level: level, TaskError: te}
	for _, n := range notifiers {
		if nerr := n.Notify(ctx, event); nerr != nil {
			e.loggerSet.Engine.Errorf("notify alert failed: %s", nerr.Error())
		}
	}
}

// recordBreakerFailure observes one persisted terminal task round.
func (e *Engine) recordBreakerFailure(task *Task, failure ...error) (BreakerStatus, bool) {
	var err error
	if len(failure) > 0 {
		err = failure[0]
	}
	if err == nil {
		err = fmt.Errorf("unspecified terminal failure")
	}
	return e.recordSmartOutcome(task, err)
}

// notifyBreakerTrip 熔断触发时的回调：打醒目日志 + 发一条 AlertCritical 告警。
//
// 走的是与任务失败**同一个** Notifier 通道，业务不用再接一套告警 ——
// 区别只在 level（critical 比 error 高一级），webhook 那边可以据此单独路由（钉钉 @全体之类）。
func (e *Engine) notifyBreakerTrip(st BreakerStatus) {
	e.emit(events.Event{
		Type: events.TypeBreakerTripped, Site: st.Site, Stage: st.Stage,
		Source: "breaker", Kind: "circuit-breaker",
		Counts: map[string]int{"failures": st.Failures, "threshold": st.Threshold},
		TS:     st.PausedAt, Attrs: map[string]string{"incident_id": st.IncidentID, "window_ns": strconv.FormatInt(int64(st.Window), 10), "window_from": st.WindowFrom.Format(time.RFC3339Nano)},
	})
	e.loggerSet.Engine.Errorf(
		"熔断触发：%s（阶段 %s，%s 窗口内 %d 次终态失败，整站硬阈值 %d）。"+
			"受保护范围已暂停；详见后台熔断分析报告及恢复状态",
		st.Reason, st.Stage, st.Window, st.Failures, st.Threshold)

	notifiers := e.getNotifiers()
	if len(notifiers) == 0 {
		return
	}
	event := AlertEvent{
		Level: AlertCritical,
		TaskError: TaskError{
			Stage:   st.Stage,
			Site:    st.Site,
			Kind:    "circuit-breaker",
			Message: st.Reason,
		},
	}
	for _, n := range notifiers {
		if err := n.Notify(e.ctx, event); err != nil {
			e.loggerSet.Engine.Errorf("notify breaker trip failed: %s", err.Error())
		}
	}
}
