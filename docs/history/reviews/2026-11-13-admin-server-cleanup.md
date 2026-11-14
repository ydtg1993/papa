# admin/server 收尾整理

<!-- version-scope -->
> **适用版本/阶段：Papa v4 演进过程中的历史工作区快照（具体基线以正文为准；未记录精确发布版的条目不推定版本号）。**
> 审查结论、路径、接口和测试结果仅适用于当次代码与环境；旧兼容约定不延续为当前契约。 当前使用方式以 [v4 手册导航](../../README.md) 为准。
<!-- /version-scope -->


## 问题与修法

目录复审发现功能实现虽已下沉，但 server 为旧单元测试保留了私有转发、重复常量和失效注释；monitor_test.go 混合多种职责。此次删除冗余转发，路由直接绑定功能 handler 并沿用 access.Wrap。公开配置、组件构造、路由、中间件按文件分工，维持同一个 server 包，不新增重复功能子包。

白名单解析/来源 IP 测试归 access，归档路径测试归 tasktrace。日志尾部两个算法测试与 logs 中实现逐字一致，删除 server 的重复副本。其余经 Monitor 路由的集成测试按主题拆分，页面组合与浏览器测试继续留在 server。原有 94 个 server 测试名称在 admin 范围内全部保留，MonitorConfig 和 Logger 声明保持一致。

测试迁移期间编译检查发现 Auth 使用 http.Handler 而非 HandlerFunc，以及测试替身和导入需同步归位；已修正并通过后台定向测试。没有修改功能 handler、前端源码或数据库行为。

## 验证

- `GOWORK=off go test -p 2 ./... -count=1` 全量普通测试通过。
- `go test -race -p 2 ./admin/... -count=1` 后台全部包竞态测试通过，无竞争报告。
- 上述运行配置真实 Chrome，后台 Node 行为及完整页面 Chrome 回归通过；本轮未配置外部 MySQL 集成测试环境，相关可选用例按默认条件跳过。
- 全仓 `go vet ./...`、`go build -mod=readonly ./...`、Grab 兼容构建通过。
- 修改文件 gofmt 检查、`git diff --check` 通过；全部公开标识符保持不变。

日志位于 `.review-tmp/server-cleanup/`。未执行 Git 写操作，保留已有改动。
