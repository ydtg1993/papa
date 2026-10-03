package crawler

import (
	"testing"

	"github.com/ydtg1993/papa/v2/internal/workerpool"
)

// 加急是一等字段：submitTo 是唯一的投递入口，四条投递路径（正常提交 / 重提交 /
// 后台重投 / 错误队列重投）都走它，所以路由只在这一个地方判。
// 池子本身不认识优先级（Tasker 接口只有 Unique），队列深度是唯一可观测的证据。
func TestSubmitToRoutesByUrgentFlag(t *testing.T) {
	pool := workerpool.NewWorkerPool[*Task](1, 8, 1)
	e := &Engine{}
	info := &stageInfo{workerPool: pool}

	if err := e.submitTo(info, &Task{URL: "a", Stage: "s"}); err != nil {
		t.Fatalf("普通任务投递失败：%v", err)
	}
	if main, urgent := pool.QueueDepths(); main != 1 || urgent != 0 {
		t.Fatalf("普通任务应进常规队列，实得 main/urgent = %d/%d", main, urgent)
	}

	if err := e.submitTo(info, &Task{URL: "b", Stage: "s", Urgent: true}); err != nil {
		t.Fatalf("加急任务投递失败：%v", err)
	}
	if main, urgent := pool.QueueDepths(); main != 1 || urgent != 1 {
		t.Fatalf("加急任务应进快车道，实得 main/urgent = %d/%d", main, urgent)
	}
}
