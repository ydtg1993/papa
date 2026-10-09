package engine

import "github.com/ydtg1993/papa/v3/config"

// errorQueueConfig 返回错误队列的生效配置（基础配置 + 运行期覆盖）。
func (e *Engine) errorQueueConfig() config.ErrorQueueConfig {
	cfg := e.cfg.ErrorQueue
	rt := e.runtime.Load()
	if rt.ErrorQueue.Enabled != nil {
		cfg.Enabled = *rt.ErrorQueue.Enabled
	}
	if rt.ErrorQueue.WorkerCount != nil {
		cfg.WorkerCount = *rt.ErrorQueue.WorkerCount
	}
	if rt.ErrorQueue.MaxRetry != nil {
		cfg.MaxRetry = *rt.ErrorQueue.MaxRetry
	}
	if rt.ErrorQueue.Interval != nil {
		cfg.Interval = rt.ErrorQueue.Interval.Duration
	}
	if rt.ErrorQueue.BatchSize != nil {
		cfg.BatchSize = *rt.ErrorQueue.BatchSize
	}
	return cfg
}

// recoverQueueConfig 返回启动恢复的生效配置（基础配置 + 运行期覆盖）。
func (e *Engine) recoverQueueConfig() config.RecoverQueueConfig {
	cfg := e.cfg.RecoverQueue
	rt := e.runtime.Load()
	if rt.RecoverQueue.Enabled != nil {
		cfg.Enabled = *rt.RecoverQueue.Enabled
	}
	if rt.RecoverQueue.WorkerCount != nil {
		cfg.WorkerCount = *rt.RecoverQueue.WorkerCount
	}
	if rt.RecoverQueue.BatchSize != nil {
		cfg.BatchSize = *rt.RecoverQueue.BatchSize
	}
	return cfg
}

// repeatQueueConfig 返回周期轮询队列的生效配置（基础配置 + 运行期覆盖）。
func (e *Engine) repeatQueueConfig() config.RepeatQueueConfig {
	cfg := e.cfg.RepeatQueue
	rt := e.runtime.Load()
	if rt.RepeatQueue.Enabled != nil {
		cfg.Enabled = *rt.RepeatQueue.Enabled
	}
	if rt.RepeatQueue.WorkerCount != nil {
		cfg.WorkerCount = *rt.RepeatQueue.WorkerCount
	}
	if rt.RepeatQueue.Interval != nil {
		cfg.Interval = rt.RepeatQueue.Interval.Duration
	}
	if rt.RepeatQueue.BatchSize != nil {
		cfg.BatchSize = *rt.RepeatQueue.BatchSize
	}
	return cfg
}
