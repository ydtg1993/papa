package main

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/go-rod/rod"
	"github.com/go-rod/rod/lib/proto"
)

const (
	// actSettle 每个动作执行后要求页面保持稳定的时长（DOM 不再变化、网络不再请求）。
	actSettle = 800 * time.Millisecond
	// actSettleTimeout 单个动作稳定等待的上限。有些页面（轮播、时钟、轮询）永不静止，
	// 超过就告警继续，而不是把调试卡死在这里。
	actSettleTimeout = 5 * time.Second
	// actDefaultTimeout 单个动作自身的超时（等元素出现、点击等），可用 --timeout 覆盖。
	actDefaultTimeout = 15 * time.Second
)

// act 一个待执行的页面动作。
type act struct {
	verb string // scroll / click / input / hover / wait / eval
	arg  string // 动作参数，原样保留供各动作自行解释
	raw  string // 原始写法，用于报错与日志
}

// actVerbs 支持的动作名，用于错误提示。
var actVerbs = []string{"scroll", "click", "input", "hover", "wait", "eval"}

// parseAct 解析 `--act <动作>:<参数>`。
func parseAct(raw string) (act, error) {
	verb, arg, found := strings.Cut(raw, ":")
	verb = strings.TrimSpace(verb)
	arg = strings.TrimSpace(arg)
	if verb == "" {
		return act{}, fmt.Errorf("缺少动作名，格式为 <动作>:<参数>，支持：%s", strings.Join(actVerbs, "/"))
	}
	if !found {
		return act{}, fmt.Errorf("动作 %q 缺少参数，格式为 %s:<参数>", verb, verb)
	}
	if arg == "" {
		return act{}, fmt.Errorf("动作 %q 的参数为空", verb)
	}
	if slices.Contains(actVerbs, verb) {
		return act{verb: verb, arg: arg, raw: raw}, nil
	}
	return act{}, fmt.Errorf("未知动作 %q，支持：%s", verb, strings.Join(actVerbs, "/"))
}

// parseActs 批量解析 --act。
func parseActs(raw []string) ([]act, error) {
	out := make([]act, 0, len(raw))
	for _, r := range raw {
		a, err := parseAct(r)
		if err != nil {
			return nil, fmt.Errorf("--act %q: %w", r, err)
		}
		out = append(out, a)
	}
	return out, nil
}

// runActs 按顺序执行动作序列；每个动作前后都保证页面已稳定。
// log 写进度与告警（stderr），不污染 stdout 的抓取结果。
func runActs(page *rod.Page, acts []act, timeout time.Duration, log io.Writer) error {
	for i, a := range acts {
		if err := runAct(page, a, timeout, log); err != nil {
			return fmt.Errorf("第 %d 个动作 %q 失败: %w", i+1, a.raw, err)
		}
		fmt.Fprintf(log, "act %d/%d %s ok\n", i+1, len(acts), a.raw)
		settle(page, a.raw, log)
	}
	return nil
}

// runAct 执行单个动作。
func runAct(page *rod.Page, a act, timeout time.Duration, log io.Writer) error {
	p := page.Timeout(timeout)
	switch a.verb {
	case "scroll":
		return actScroll(p, a.arg)
	case "click":
		el, err := element(p, a.arg)
		if err != nil {
			return err
		}
		// 目标可能在视口外或被子元素遮挡，先滚入视口再点
		if err := el.ScrollIntoView(); err != nil {
			return fmt.Errorf("滚动到元素: %w", err)
		}
		return el.Click(proto.InputMouseButtonLeft, 1)
	case "input":
		sel, text, ok := strings.Cut(a.arg, "=")
		sel = strings.TrimSpace(sel)
		if !ok || sel == "" {
			return fmt.Errorf("格式为 input:<选择器>=<文本>")
		}
		el, err := element(p, sel)
		if err != nil {
			return err
		}
		if err := el.SelectAllText(); err != nil {
			return fmt.Errorf("清空原文本: %w", err)
		}
		return el.Input(text)
	case "hover":
		el, err := element(p, a.arg)
		if err != nil {
			return err
		}
		return el.Hover()
	case "wait":
		return actWait(p, a.arg)
	case "eval":
		return actEval(p, a.arg, log)
	}
	return fmt.Errorf("未知动作 %q", a.verb)
}

// actEval 在页面上下文执行一段 JS，并把返回值打到 log。
// rod 的 Eval 会把它当函数调用（f.apply），所以这里用一层 eval 包装，
// 这样 <js> 既能写表达式（document.title）也能写语句（document.title='x'）。
func actEval(page *rod.Page, js string, log io.Writer) error {
	res, err := page.Eval(`(code) => eval(code)`, js)
	if err != nil {
		return err
	}
	// 字符串去掉引号，对象/数字保持 JSON 原样
	out := res.Value.String()
	if s, err := strconv.Unquote(out); err == nil {
		out = s
	}
	fmt.Fprintf(log, "  eval => %s\n", out)
	return nil
}

// actScroll 支持 scroll:bottom / scroll:top / scroll:<屏数> / scroll:<选择器>。
func actScroll(page *rod.Page, arg string) error {
	switch arg {
	case "bottom":
		_, err := page.Eval(`() => window.scrollTo(0, Math.max(document.body.scrollHeight, document.documentElement.scrollHeight))`)
		return err
	case "top":
		_, err := page.Eval(`() => window.scrollTo(0, 0)`)
		return err
	}
	if n, err := strconv.Atoi(arg); err == nil {
		if n <= 0 {
			return fmt.Errorf("scroll 屏数必须为正整数，实际 %d", n)
		}
		_, err := page.Eval(`(n) => window.scrollBy(0, window.innerHeight * n)`, n)
		return err
	}
	el, err := element(page, arg)
	if err != nil {
		return err
	}
	return el.ScrollIntoView()
}

// waitIsDuration 判断 wait:<参数> 是等待时长（true）还是 CSS 选择器（false）。
// 合法选择器不可能是合法时长（"2s"/"500ms" 都不是合法 CSS），因此这个判别不会歧义。
func waitIsDuration(arg string) bool {
	_, err := time.ParseDuration(arg)
	return err == nil
}

// actWait 支持 wait:<时长>（固定等待）与 wait:<选择器>（等元素出现）。
func actWait(page *rod.Page, arg string) error {
	if waitIsDuration(arg) {
		d, _ := time.ParseDuration(arg)
		if d < 0 {
			return fmt.Errorf("等待时长不能为负: %s", arg)
		}
		time.Sleep(d)
		return nil
	}
	_, err := element(page, arg)
	return err
}

// element 等待并返回选择器匹配的第一个元素。
func element(page *rod.Page, selector string) (*rod.Element, error) {
	if selector == "" {
		return nil, fmt.Errorf("选择器为空")
	}
	el, err := page.Element(selector)
	if err != nil {
		return nil, fmt.Errorf("元素 %q 未出现（选择器写错或元素始终没渲染）: %w", selector, err)
	}
	return el, nil
}

// waitEnter 交还控制权给人工（--show / --devtools）：浏览器窗口里点完、拉完再回终端回车，
// 之后才读取最终 HTML/截图。stdin 不可读（管道、重定向）时立即返回，不阻塞。
func waitEnter() error {
	fmt.Fprintln(os.Stderr, "浏览器已打开：请在窗口中完成操作，完成后回到终端按回车导出结果...")
	_, err := bufio.NewReader(os.Stdin).ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return fmt.Errorf("读取回车: %w", err)
	}
	return nil
}

// settle 等页面稳定，避免下拉/点击后立刻取到半截数据（懒加载最常见）。
// 超时只告警不中断：页面可能一直在动，卡住没有意义，用户可用 --act wait:<时长> 显式控制。
func settle(page *rod.Page, what string, log io.Writer) {
	if err := page.Timeout(actSettleTimeout).WaitStable(actSettle); err != nil {
		fmt.Fprintf(log, "  提示: %s 后页面未在 %s 内稳定(%v)，已继续；必要时补 --act wait:<时长>\n",
			what, actSettle, err)
	}
}
