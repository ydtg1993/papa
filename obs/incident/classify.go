package incident

import (
	"regexp"
	"strings"
)

var httpCode = regexp.MustCompile(`(?i)\b(?:http(?:/\d(?:\.\d)?)?|status(?:[_ ]?code)?)[ :=]+([45]\d\d)\b`)

func Classify(kind, message string) (string, string, string) {
	s := strings.ToLower(message)
	has := func(parts ...string) bool {
		for _, p := range parts {
			if strings.Contains(s, p) {
				return true
			}
		}
		return false
	}
	switch {
	case has("processsingleton", "lock file can not", "user data directory is already in use"):
		return "浏览器目录锁", "浏览器用户目录可能已被占用或无法创建锁文件", "检查同一 profile 是否被其他浏览器占用、目录权限和浏览器启动日志。"
	case has("access is denied", "permission denied", "eacces"):
		return "访问权限", "进程可能没有访问文件、目录或系统资源的权限", "对照错误中的资源路径和运行用户，检查读写权限；不能仅据此认定是浏览器故障。"
	case has("browser binary", "executable file not found", "failed to get the debug url", "cannot find chrome"):
		return "浏览器启动", "浏览器可执行文件、启动参数或运行环境可能异常", "核实浏览器路径与版本，查看完整启动 stderr；debug URL 缺失本身不能证明权限问题。"
	case has("no space left", "disk full", "out of memory", "cannot allocate memory"):
		return "本机资源不足", "磁盘或内存分配失败", "检查错误所指资源、可用磁盘空间和进程内存，再核对并发及临时目录。"
	case has("sqlstate", "deadlock", "duplicate entry", "database is locked", "too many connections"):
		return "数据库", "数据库连接、事务冲突或约束可能导致失败", "按原文错误码检查数据库状态、连接池、锁等待或唯一约束；不要盲目重试。"
	case has("no such host", "name resolution", "dns"):
		return "DNS 解析", "域名解析可能失败", "在相同运行环境检查目标域名解析和代理的 DNS 配置。"
	case has("x509:", "tls handshake", "certificate"):
		return "TLS / 证书", "证书校验或 TLS 握手可能失败", "核对系统时间、证书链、代理及错误原文；不要直接关闭证书校验。"
	}
	if m := httpCode.FindStringSubmatch(message); len(m) > 1 {
		switch m[1] {
		case "429":
			return "HTTP 429", "服务端报告请求过多", "检查 Retry-After、站点频率和共享出口请求量。"
		case "401", "403":
			return "HTTP " + m[1], "身份凭据、访问策略或目标端拒绝访问；不能直接认定反爬", "检查响应正文、登录有效性、权限和出口条件。"
		case "404", "410":
			return "HTTP " + m[1], "目标资源不存在或已经移除", "核对任务 URL、重定向和入口生成规则。"
		default:
			return "HTTP " + m[1], "目标服务或中间代理返回错误状态", "检查响应正文和目标/代理可用性，对照同一时间的错误分布。"
		}
	}
	switch {
	case has("timeout", "timed out", "deadline exceeded"):
		return "超时", "网络、目标服务、浏览器等待或本机排队可能超时", "确认超时发生在哪一步，核对耗时、目标响应与资源负载；不要仅凭超时判断并发过高。"
	case has("connection refused", "connection reset", "network is unreachable", "unexpected eof"):
		return "网络连接", "目标连接或代理链路可能异常", "核对目标端口、代理可达性和同时间连接错误。"
	case kind == "structure":
		return "页面解析", "页面结构与解析预期不符，也可能返回登录页或错误页", "查看失败页面归档、响应状态和内容，再核对选择器及链接规则。"
	case kind == "protected" || kind == "access_restricted":
		return "访问受限", "抓取器报告访问受限，具体机制尚未确认", "核对响应页面、账号状态及站点访问要求。"
	default:
		return "原因待确认", "现有错误文本不足以确定原因", "从代表任务和脱敏错误原文入手，查看对应阶段日志与页面归档。"
	}
}
