# Papa 错误队列处理手册

失败任务统一由错误队列处理；周期队列只处理成功任务。队列始终运行，不提供 enabled 开关。

## 1. 配置

```yaml
error_queue:
  worker_count: 1
  interval: "4h"
  max_retry: 3
  batch_size: 100
```

interval=0 默认4h，worker_count=0 默认1，batch_size=0 默认1000；负数拒绝。max_retry=0 表示不限制自动再处理轮数，但不能绕过 NoRetry。

## 2. 唯一资格判据

自动扫描及重投 UPDATE 均要求 status=failed、retry_blocked=false、max_retry>0 时 auto_retry_count 尚未达到上限。UPDATE 还检查查询时的 repeat/reprocess 代次。周期队列（包括强制轮询）不接管失败任务，过期 next_repeat_at 不能绕过限制。

NoRetry（WrapNoRetry、WrapNoRetryKind、自定义 Retryable() false）立即停止当前轮的内部重试，终态落库时持久化 retry_blocked=true。普通失败不会清除已有禁止标记。错误类别字符串不参与资格判定。临时故障应返回普通 error 并设置阶段退避/错误扫描间隔；不能使用 NoRetry 表达“现在不重试、稍后自动重试”。

## 3. 额度与代次分离

| 字段 | 含义 |
| --- | --- |
| retry | 当前任务内部重试记录；阶段 MaxAttempts 控制每轮尝试上限 |
| auto_retry_count | 当前连续失败期间已发起的自动再处理轮数 |
| reprocess | 自动与手动重投代次，保持单调，不作为预算，不清零 |
| repeat | 周期轮询代次，与 reprocess 共同拒绝旧快照 |
| retry_blocked | false 允许自动重试；true 禁止；NULL 历史策略未知，默认禁止 |

首次执行失败不扣自动预算。max_retry=3 允许此后最多3轮自动再处理，每轮可以包含阶段 MaxAttempts 次尝试；成功终态在同一 UPDATE 内清零 auto_retry_count、设 retry_blocked=false，未来周期失败开启新预算。失败与暂停不会清零。自动重投的额度增加与 failed→pending、reprocess+1 合并成一条条件 UPDATE，不增加独立写入。

## 4. 手动与生命周期

队列面板“立即执行”及 ProcessErrorQueue 仍使用自动资格，不绕过上限。单条 RetryTask / 后台“重投”是显式手动执行一轮：越过自动限制，但保留预算和禁止标记；执行成功才重置，失败继续受限制。手动重投递增 reprocess，不增加 auto_retry_count。后台“标失败”同时禁止自动重试，防止被队列立刻复活。

暂停期间已接受重投保持 pending。恢复、关停取消、重启恢复不重复扣额度；重试轮次在从 failed 成功转为 pending 时已扣一次。旧 pending/processing 的未知标记在认领时设为允许；手动重投历史 failed 先把未知标记物化为禁止，所以关停重启也不会解除限制。

## 5. 迁移

升级前停旧进程，执行 papa migrate（crawler --migrate）。新增 auto_retry_count（默认0）和可空 retry_blocked（默认NULL）。新任务通过模型创建 hook 显式写false；旧行不做全表回填，不从错误文本推断 NoRetry，也不把历史 reprocess 换算为预算。旧 failed 保持待人工确认，单条手动重投成功即可解除；旧 success 正常轮询。重复迁移不覆盖已产生的计数或策略。大表 DDL 的锁和耗时需安排停机窗口。

配套：[REPEAT_QUEUE.md](./REPEAT_QUEUE.md)、[RECOVER_QUEUE.md](./RECOVER_QUEUE.md)。

自动重试查询使用新增复合索引 idx_task_auto_retry(status, retry_blocked, auto_retry_count)，避免历史被禁止失败行反复参与候选扫描；大表建索引需预留维护窗口。
