package papa

import "github.com/ydtg1993/papa/v3/engine"

// 本文件是**任务与引擎门面**：写 fetcher 要用到的全部类型。

// Engine 爬虫引擎。
//
// 它是这个门面里**唯一还留在 engine 包**的重头 —— Fetcher.FetchHandler 的签名里带着
// *Engine，两者绑在一起，没法下沉到 core（见 core 的包注释）。
type Engine = engine.Engine

// Fetcher 爬虫业务逻辑接口。
type Fetcher = engine.Fetcher

// Task 任务结构。
type Task = engine.Task

// StageConfig 阶段配置。
type StageConfig = engine.StageConfig
