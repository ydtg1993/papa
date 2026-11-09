# 临时资源与测试库

> 面向：需要临时数据隔离、临时资源销毁或自动化真库回归的业务模块。无需新增框架表或迁移。

## 职责

Papa 提供 `pkg/temporary` 的路径约束、销毁顺序与重启清理，以及 `pkg/testdb` 的独占测试库生命周期。业务负责表结构、数据行归属、执行排空和人工确认。框架不会扫描库名前缀自动删除数据库，不会启动采集。

## 文件隔离

```go
files := temporary.Files{Parent: configuredRoot, Namespace: "experiments"}
root, err := files.Open(true)
if err != nil { return err }
defer root.Close()
```

父目录由宿主配置且必须已存在，Namespace 是单层小写名称。每个实验使用规范 UUID 子目录，文件也使用 UUID。可信宿主通过返回的 `os.Root` 创建和写入；根目录与句柄不得交由不可信输入控制。`FilePath(id,fileID)` 只检查有效ID、归属路径与普通文件；内容大小、哈希和业务访问权限由调用者核验。返回路径不是防止后续替换的持久文件句柄，调用者须锁定本实验读写/清理，保证目录不被不可信本地进程改写。

`Remove(id)` 只清理该 UUID 目录，已不存在时成功；拒绝符号链接、Windows reparse point、越界名称和非法父目录。不会删除相邻实验、共享样本或 namespace 根目录。它不负责取消仍在写入的作业，须先停止并排空。

## 数据与文件的一致清理

业务实现 `temporary.CleanupStore`：

| 方法 | 业务必须保证 |
| --- | --- |
| `BeginCleanup(ctx,id,actor) (done,error)` | 核对归属与可清理状态，将已授权删除意图持久化；完成过才返回 done=true |
| `FinishCleanup(ctx,id) error` | 在业务事务中删除仅属于该ID的临时行，并标记完成；失败不能标记成功 |
| `PendingCleanup(ctx) ([]CleanupRequest,error)` | 只返回已经授权且未完成的删除，包括原操作人；不返回运行中或未确认实验 |

```go
cleaner := temporary.Cleaner{Files: files, Store: businessCleanupStore}
// 仅在人工确认且作业排空后调用；同一个ID的访问由宿主串行保护。
err := cleaner.Cleanup(ctx, experimentID, operator)
// 启动时仅恢复已经授权但未完成的清理。
err = cleaner.Recover(ctx)
```

顺序固定为：保存删除意图 → 删除文件 → 事务删除数据/标记完成。文件删除失败不进入完成事务；事务失败保留意图，下次恢复接受文件已不存在。没有跨文件系统与数据库的原子事务，也不新增后台调度器。父目录配置应在清理恢复期间保持一致。保存业务数据、关闭页面、测试失败本身都不是删除授权。

## 自动化测试库

`testdb.Create(ctx, adminDB, name)` 接收实现 ExecContext 的连接，返回 `cleanup func() error`。库名必须是1–64位小写 SQL 标识符且含独立 `test` 字段（例如 `fixture_test_a1`），不得传正式库。框架不内置站点名或业务库名。

- 使用独占 `CREATE DATABASE`；同名库存在则失败，不接管、不返回清理权限。
- 成功后立即通过 `defer` 或 `t.Cleanup` 登记 cleanup。按逆序先排空作业、关闭业务连接，最后清理测试库、关闭管理连接。
- 测试通过、失败或取消均调用 cleanup；清理使用独立30秒上下文。清理失败必须报告，允许显式再次调用；成功重复调用不再发 SQL。
- 强杀进程不会执行 Go 清理回调，不能承诺此时自动删除；核对遗留归属后人工处理，不能按模糊前缀删除。
- 日志可保留，测试数据不默认长期保留。

本包提供文件隔离与清理机制；资源归属、作业生命周期和界面由业务实现。新项目通过公开包调用；`papa new` 和 `papa upgrade` 会同步本手册，不自动给业务增加模型、测试库或采集任务。
