package reportrunner

import (
	"github.com/ydtg1993/papa/v4/obs"
	"sort"
	"strconv"
)

// Tables are bounded projections of owned signal state, never database queries.
func signalTables(a *aggregate) []obs.Table {
	num := func(n uint64) string { return strconv.FormatUint(n, 10) }
	out := []obs.Table{{Title: "收到的信号数量（不等同任务总量）", Columns: []string{"范围", "接收", "完成信号", "失败信号", "重试信号"}, Rows: [][]string{{"本日", num(a.counts.Received), num(a.counts.Finished), num(a.counts.Failed), num(a.counts.Retry)}, {"本进程", num(a.run.Received), num(a.run.Finished), num(a.run.Failed), num(a.run.Retry)}}}}
	stages := obs.Table{Title: "最后收到的阶段快照（可能滞后）", Columns: []string{"阶段", "队列长度", "执行中", "worker"}}
	keys := make([]string, 0, len(a.stages))
	for k := range a.stages {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		v := a.stages[k]
		stages.Rows = append(stages.Rows, []string{k, strconv.Itoa(v.QueueLen), strconv.Itoa(v.InProgress), strconv.Itoa(v.Workers)})
	}
	if len(stages.Rows) > 0 {
		out = append(out, stages)
	}
	queues := obs.Table{Title: "最后收到的治理队列快照（可能滞后）", Columns: []string{"队列", "积压", "本次处理", "运行中"}}
	keys = keys[:0]
	for k := range a.queues {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		v := a.queues[k]
		queues.Rows = append(queues.Rows, []string{k, strconv.FormatInt(v.Backlog, 10), strconv.FormatInt(v.RunProcessed, 10), strconv.FormatBool(v.Running)})
	}
	if len(queues.Rows) > 0 {
		out = append(out, queues)
	}
	samples := obs.Table{Title: "最近失败信号样本（最多16条）", Columns: []string{"接收时间", "站点", "阶段", "错误"}}
	for _, s := range a.samples {
		samples.Rows = append(samples.Rows, []string{s.At.Format("15:04:05"), s.Site, s.Stage, s.Error})
	}
	if len(samples.Rows) > 0 {
		out = append(out, samples)
	}
	return out
}
