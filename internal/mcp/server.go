package mcp

import (
	"net/http"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/ydtg1993/papa/internal/crawler"
)

// Handler 构建 Papa 的 MCP 服务，并以 streamable HTTP handler 形式返回。
// 挂载到统一 HTTP server 的 /mcp 路径即可。
func Handler(engine *crawler.Engine) http.Handler {
	srv := sdkmcp.NewServer(&sdkmcp.Implementation{Name: "papa", Version: "1.0.0"}, nil)

	sdkmcp.AddTool(srv, &sdkmcp.Tool{
		Name:        "submit_task",
		Description: "提交爬取任务到指定阶段(写入数据库并进入对应工作池)",
	}, submitTaskTool(engine))

	sdkmcp.AddTool(srv, &sdkmcp.Tool{
		Name:        "list_stages",
		Description: "列出所有已配置的爬取阶段及其并发/重试参数",
	}, listStagesTool(engine))

	sdkmcp.AddTool(srv, &sdkmcp.Tool{
		Name:        "get_stats",
		Description: "获取各阶段任务执行统计(成功/失败/耗时)",
	}, getStatsTool(engine))

	sdkmcp.AddTool(srv, &sdkmcp.Tool{
		Name:        "list_tasks",
		Description: "按阶段/状态查询已入库的爬取任务",
	}, listTasksTool(engine))

	sdkmcp.AddTool(srv, &sdkmcp.Tool{
		Name:        "resubmit_task",
		Description: "重新提交一个已入库但未完成的任务",
	}, resubmitTaskTool(engine))

	sdkmcp.AddTool(srv, &sdkmcp.Tool{
		Name:        "recover_tasks",
		Description: "恢复超时未完成(pending/processing 且长时间未更新)的任务",
	}, recoverTasksTool(engine))

	return sdkmcp.NewStreamableHTTPHandler(func(*http.Request) *sdkmcp.Server { return srv }, nil)
}
