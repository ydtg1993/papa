package config

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

/* ---------- 辅助 ---------- */

func writeFile(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

// assertPanicContains 断言 f() 会 panic，且 panic 信息里出现全部 want 片段。
// 校验层的报错是给人看的，所以片段通常是"键路径"加"为什么"。
func assertPanicContains(t *testing.T, f func(), want ...string) {
	t.Helper()
	defer func() {
		r := recover()
		if r == nil {
			t.Fatalf("应当 panic，want 片段 %v", want)
		}
		msg := fmt.Sprint(r)
		for _, w := range want {
			if !strings.Contains(msg, w) {
				t.Fatalf("panic 信息里缺 %q，实得：\n%s", w, msg)
			}
		}
	}()
	f()
}

// assertPanicNamesKey 断言 f() 会 panic 且点名了配置键 —— 不然用户不知道该去哪改。
func assertPanicNamesKey(t *testing.T, key string, f func()) {
	t.Helper()
	assertPanicContains(t, f, key)
}

// baseConfig 一份「最小但合法」的配置。**log.dir 与两个连接池键是必填的**
// （漏写不是中性值：前者去写盘根，后者在 database/sql 里变成「不限」），
// 所以每个夹具都得带上，各用例只追加自己要测的那一段。
const baseConfig = "log:\n  dir: ./logs\ndb:\n  max_idle_conns: 10\n  max_open_conns: 100\n"

// loadString 把一段 YAML 写进临时文件再 Load；期望成功。
func loadString(t *testing.T, body string) *Config {
	t.Helper()
	p := filepath.Join(t.TempDir(), "config.yaml")
	writeFile(t, p, body)
	cfg, err := Load(p)
	if err != nil {
		t.Fatalf("Load 不该失败：%v\n配置：\n%s", err, body)
	}
	return cfg
}

// panicOn 用一段 YAML 触发 Load，断言它 panic 并返回 panic 信息。
func panicOn(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "config.yaml")
	writeFile(t, p, body)
	var msg string
	func() {
		defer func() {
			r := recover()
			if r == nil {
				t.Fatalf("应当 panic：\n%s", body)
			}
			msg = fmt.Sprint(r)
		}()
		Load(p)
	}()
	return msg
}

/* ---------- 值域规则 ---------- */

// 每条规则都必须回答三态，这是这一层最容易做错的地方：
//
//	① 没写（零值）→ 用默认值，**合法**
//	② 写了且合法 → 放行
//	③ 写了但越界 → 拒绝
//
// 漏掉 ① 就会把一堆可选键变成必填，直接炸掉所有老配置 —— 所以每条规则
// 都在这里被强制要求给出一个「absent 也合法」的夹具。
//
// **三个必填键不在这张表里**（它们没有 ① 那一态）：`log.dir`、`db.max_idle_conns`、
// `db.max_open_conns` —— 见 TestRequiredKeys。夹具一律给**完整配置**而不是往公共前缀上追加：
// 公共前缀里已经有 `db:` 段，再追加一个同名的会变成 YAML 重复键。
func TestRuleBoundaries(t *testing.T) {
	cases := []struct {
		name   string
		key    string
		absent string // 键整个不出现（但仍满足三个必填键）
		ok     string
		bad    string
	}{
		{
			name:   "crawler.breaker.window",
			key:    "crawler.breaker.window",
			absent: baseConfig,
			ok:     baseConfig + "crawler:\n  breaker:\n    enabled: true\n    threshold: 1\n    window: 5m\n",
			bad:    baseConfig + "crawler:\n  breaker:\n    enabled: true\n    threshold: 1\n    window: 10ns\n",
		},
		{
			name:   "crawler.breaker.threshold",
			key:    "crawler.breaker.threshold",
			absent: baseConfig, // 整个 breaker 段不写 = 没开熔断，合法
			ok:     baseConfig + "crawler:\n  breaker:\n    enabled: true\n    threshold: 50\n",
			bad:    baseConfig + "crawler:\n  breaker:\n    enabled: true\n", // 开了却不给阈值
		},
		{
			// browser.binary 的三种取值都合法；拼错（或写成 auto/default 这类
			// "看起来像默认"的词）必须拒绝 —— 否则值悄悄不生效，二进制仍按 rod 走。
			name:   "browser.binary",
			key:    "browser.binary",
			absent: baseConfig, // 留空 = 有 browser_path 走 path，没有走 rod
			ok:     baseConfig + "browser:\n  binary: rod\n",
			bad:    baseConfig + "browser:\n  binary: auto\n",
		},
		{
			name: "browser.flags",
			key:  "browser.flags",
			// 键里带 "=" 是典型写法错误：它会被当成开关名的一部分，
			// 而不是像 --blink-settings=... 那样拆开。
			absent: baseConfig,
			ok:     baseConfig + "browser:\n  flags:\n    disable-gpu:\n    renderer-process-limit: \"2\"\n",
			bad:    baseConfig + "browser:\n  flags:\n    \"blink-settings=imagesEnabled=false\": \"\"\n",
		},
		{
			name:   "proxy.refresh_interval",
			key:    "proxy.refresh_interval",
			absent: baseConfig + "proxy:\n  api_url: \"\"\n", // 没有 api_url 就不会起刷新协程，这个值用不上
			ok:     baseConfig + "proxy:\n  api_url: http://example.com/proxies\n  refresh_interval: 500s\n",
			bad:    baseConfig + "proxy:\n  api_url: http://example.com/proxies\n  refresh_interval: 0s\n",
		},
		{
			name:   "crawler.archive.dir",
			key:    "crawler.archive.dir",
			absent: baseConfig, // 没开归档，用不上目录
			ok:     baseConfig + "crawler:\n  archive:\n    enabled: true\n    dir: ./logs/fetcher-html\n",
			bad:    baseConfig + "crawler:\n  archive:\n    enabled: true\n",
		},
		{
			name:   "crawler.archive.mode",
			key:    "crawler.archive.mode",
			absent: baseConfig, // 留空 = failure
			ok:     baseConfig + "crawler:\n  archive:\n    mode: always\n",
			bad:    baseConfig + "crawler:\n  archive:\n    mode: sometimes\n",
		},
		{
			name:   "crawler.archive.max_file_mb",
			key:    "crawler.archive.max_file_mb",
			absent: baseConfig, // 留空 = 8MB
			ok:     baseConfig + "crawler:\n  archive:\n    max_file_mb: 8\n",
			bad:    baseConfig + "crawler:\n  archive:\n    max_file_mb: 8192\n",
		},
		{
			name:   "db.log_level",
			key:    "db.log_level",
			absent: baseConfig, // 留空 = 按 app.env 推，合法
			// db 段要整块写出来：公共前缀里已有一个 db:，再追加会变成 YAML 重复键
			ok:  "log:\n  dir: ./logs\ndb:\n  max_idle_conns: 10\n  max_open_conns: 100\n  log_level: warn\n",
			bad: "log:\n  dir: ./logs\ndb:\n  max_idle_conns: 10\n  max_open_conns: 100\n  log_level: verbose\n",
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			// ① 没写 → 用默认值，必须合法
			loadString(t, c.absent)
			// ② 写了且合法
			loadString(t, c.ok)
			// ③ 写了但越界 → panic，且点名键
			assertPanicNamesKey(t, c.key, func() { Load(mustWrite(t, c.bad)) })
		})
	}
}

// mustWrite 把 YAML 写进临时文件并返回路径（给需要在闭包里触发 panic 的用例用）。
func mustWrite(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "config.yaml")
	writeFile(t, p, body)
	return p
}

// htmlOn 造一个"静态抓取开着"的配置，extra 是要塞进 html 段的额外行（可为空）。
func htmlOn(extra string) string {
	return baseConfig + "html:\n  enable: true\n  timeout: 15s\n" + extra
}

// browserOn 造一个"浏览器池开着"的配置，extra 是要塞进 browser 段的额外行（可为空）。
func browserOn(extra string) string {
	return baseConfig + "browser:\n  enable: true\n" + extra
}

// **零值危险的键**：三个必填（`log.dir` / `db.max_idle_conns` / `db.max_open_conns` ——
// 它们**没有**「没写也合法」那一态），外加一族「开着才必填」（如 `html.max_body_size`、
// `browser.pool_size`：关掉对应开关就用不上）。共同点是零值不是中性值，而是一个危险值：
//
//   - log.dir 空串 → 拼出文件系统根下的 sys.log（没权限就静默一条日志都没有）
//   - db.max_idle_conns / max_open_conns 漏写 → 0，而 database/sql 把 0 当成
//     「不保留空闲连接」/「不限」。写 0 与不写是同一个数、分不开（除非把字段改成 *int），
//     所以范围校验对"漏配就静默打满 MySQL"这条毛病是**空的** —— 只能收成必填。
//   - html.max_body_size 漏写 → 0，`LimitReader(body, 0+1)` 把上限算成 1 字节，
//     每一次抓取都报「页面太大」。只在 `enable: true` 时要求（关掉就用不上它）。
func TestRequiredKeys(t *testing.T) {
	pool := func(idle, open int) string {
		return fmt.Sprintf("log:\n  dir: ./logs\ndb:\n  max_idle_conns: %d\n  max_open_conns: %d\n", idle, open)
	}

	for _, c := range []struct {
		name string
		key  string
		body string
	}{
		{"log.dir 漏写", "log.dir", "app:\n  env: dev\n"},
		{"log.dir 写成空白", "log.dir", "log:\n  dir: '   '\n"},
		{"log.dir 漏写（有 db 段）", "log.dir", "db:\n  max_idle_conns: 10\n  max_open_conns: 100\n"},
		{"max_idle_conns 漏写", "db.max_idle_conns", "log:\n  dir: ./logs\ndb:\n  max_open_conns: 100\n"},
		{"max_open_conns 漏写", "db.max_open_conns", "log:\n  dir: ./logs\ndb:\n  max_idle_conns: 10\n"},
		{"两个都漏写（先报 idle）", "db.max_idle_conns", "log:\n  dir: ./logs\n"},
		{"max_idle_conns 写 0", "db.max_idle_conns", pool(0, 100)},
		{"max_open_conns 写 0", "db.max_open_conns", pool(10, 0)},
		{"max_idle_conns 负数", "db.max_idle_conns", pool(-1, 100)},
		{"max_open_conns 负数", "db.max_open_conns", pool(10, -1)},
		{"max_idle_conns 超上界", "db.max_idle_conns", pool(101, 100)},
		{"max_open_conns 超上界", "db.max_open_conns", pool(10, 101)},

		// html.max_body_size 也是"开着的时候必填"：它的零值 0 会让 LimitReader 只放 1 字节，
		// 于是每一次抓取都报「页面太大」。关掉 html 时这个值用不上，规则整条跳过（见下面的正向用例）。
		{"max_body_size 漏写（html.enable=true）", "html.max_body_size", htmlOn("")},
		{"max_body_size 写 0", "html.max_body_size", htmlOn("  max_body_size: 0\n")},
		{"max_body_size 写 10（想当 10MB，实际 10 字节）", "html.max_body_size", htmlOn("  max_body_size: 10\n")},
		{"max_body_size 低于下界 100KB", "html.max_body_size", htmlOn("  max_body_size: 102399\n")},
		{"max_body_size 超上界 64MB", "html.max_body_size", htmlOn("  max_body_size: 67108865\n")},

		// browser.pool_size 与 html.max_body_size 同一个形状：「开着的时候必填」。
		// 零值 0 让池容量为 0 —— Get 的两条快路径（复用空闲 / 未达上限则新建）都带
		// `limit > 0` 前缀，于是全跳过，直接停在 cond.Wait 上永久阻塞且**不报错**。
		{"pool_size 漏写（browser.enable=true）", "browser.pool_size", browserOn("")},
		{"pool_size 写 0", "browser.pool_size", browserOn("  pool_size: 0\n")},
		{"pool_size 写负数", "browser.pool_size", browserOn("  pool_size: -1\n")},
	} {
		t.Run(c.name, func(t *testing.T) {
			assertPanicNamesKey(t, c.key, func() { Load(mustWrite(t, c.body)) })
		})
	}

	// 两端都要放行：1 与 100 都是合法端点，idle == open 也合法
	loadString(t, pool(1, 1))
	loadString(t, pool(100, 100))
	loadString(t, baseConfig)

	// html.max_body_size：模板值（10MB）与区间两端都要放行
	loadString(t, htmlOn("  max_body_size: 10485760\n")) // 模板值
	loadString(t, htmlOn("  max_body_size: 102400\n"))   // 下界
	loadString(t, htmlOn("  max_body_size: 67108864\n")) // 上界
	// 关掉静态抓取时这个值根本不参与 —— 不许一个用不着的东西把人卡在启动上
	loadString(t, baseConfig+"html:\n  enable: false\n  max_body_size: 1\n")

	// browser.pool_size：模板值要放行；**direct_pool_size 留 0 也必须合法**（不用直连池是常态，
	// GetDirect 自己会报"direct browsers not configured"，不需要启动期拦）
	loadString(t, browserOn("  pool_size: 3\n"))
	loadString(t, browserOn("  pool_size: 3\n  direct_pool_size: 0\n"))
	// 关掉浏览器池时 pool_size 根本不参与
	loadString(t, baseConfig+"browser:\n  enable: false\n  pool_size: 0\n")
}

// browser.binary 与 browser_path 的组合规则：
//   - 留空 binary：给了 browser_path 按 path，没给按 rod（都合法）；
//   - rod / system：不能再给 browser_path（两个来源互相矛盾，静默选一个都是坑）；
//   - path：必须给 browser_path。
//
// system 能不能真的找到浏览器不在配置层判断 —— 那是环境，建池时才探测
// （见 pkg/browser 的 resolveBrowserBinary）。
func TestBrowserBinaryCombinations(t *testing.T) {
	// 留空 + browser_path：显式路径
	cfg := loadString(t, baseConfig+"browser:\n  browser_path: /usr/bin/chromium\n")
	if cfg.Browser.Binary != "" || cfg.Browser.BrowserPath != "/usr/bin/chromium" {
		t.Fatalf("解析结果不对：binary=%q path=%q", cfg.Browser.Binary, cfg.Browser.BrowserPath)
	}
	// 留空 + 不给路径：Rod 自动下载
	loadString(t, baseConfig+"browser: {}\n")
	// system：配置层放行（找不到系统浏览器由建池时报）
	loadString(t, baseConfig+"browser:\n  binary: system\n")
	// path + 路径
	loadString(t, baseConfig+"browser:\n  binary: path\n  browser_path: /usr/bin/chromium\n")

	for _, c := range []struct {
		name string
		body string
	}{
		{"path 却没给 browser_path", baseConfig + "browser:\n  binary: path\n"},
		{"path 的 browser_path 只有空白", baseConfig + "browser:\n  binary: path\n  browser_path: '   '\n"},
		{"rod 又给了 browser_path", baseConfig + "browser:\n  binary: rod\n  browser_path: /usr/bin/chromium\n"},
		{"system 又给了 browser_path", baseConfig + "browser:\n  binary: system\n  browser_path: /usr/bin/chromium\n"},
	} {
		t.Run(c.name, func(t *testing.T) {
			assertPanicNamesKey(t, "browser.binary", func() { Load(mustWrite(t, c.body)) })
		})
	}
}

// browser.flags 的值语义：空值 = 布尔开关，非空 = --name=value。
// YAML 里 `disable-gpu:`（不带值）解出来是 nil，mapstructure 要落成空串 ——
// 这条钉住它，免得将来换成别的解码方式时布尔开关静默变成 "--disable-gpu=<nil>"。
func TestBrowserFlagsParsed(t *testing.T) {
	cfg := loadString(t, baseConfig+`browser:
  flags:
    disable-gpu:
    disable-extensions: ""
    blink-settings: imagesEnabled=false
    renderer-process-limit: "2"
`)
	want := map[string]string{
		"disable-gpu":            "",
		"disable-extensions":     "",
		"blink-settings":         "imagesEnabled=false",
		"renderer-process-limit": "2",
	}
	if len(cfg.Browser.Flags) != len(want) {
		t.Fatalf("flags = %#v，want %#v", cfg.Browser.Flags, want)
	}
	for k, v := range want {
		if got, ok := cfg.Browser.Flags[k]; !ok || got != v {
			t.Fatalf("flags[%q] = %q（存在=%v），want %q；全部：%#v", k, got, ok, v, cfg.Browser.Flags)
		}
	}
}

// rod- 前缀是 launcher 的内部参数（rod-bin / rod-leakless …）：它们不进 Chromium
// 命令行，写进 browser.flags 只会静默改掉框架自己的启动行为，所以直接拒绝。
func TestBrowserFlagsRejectsRodInternal(t *testing.T) {
	assertPanicNamesKey(t, "browser.flags", func() {
		Load(mustWrite(t, baseConfig+"browser:\n  flags:\n    rod-bin: /usr/bin/chromium\n"))
	})
}

// idle > open：sql.DB.SetMaxIdleConns 的文档明写会把 n 压到 maxOpenConns ——
// 所以配置里那个 idle 是个谎话（说了不数），而且没有任何提示。
// 与"漏写变成不限"是同一类静默，只是方向相反。
func TestIdleConnsCannotExceedOpenConns(t *testing.T) {
	msg := panicOn(t, "log:\n  dir: ./logs\ndb:\n  max_idle_conns: 50\n  max_open_conns: 10\n")
	for _, want := range []string{"db.max_idle_conns", "不能大于 db.max_open_conns", "静默压到"} {
		if !strings.Contains(msg, want) {
			t.Fatalf("panic 信息里缺 %q，实得：\n%s", want, msg)
		}
	}
}

/* ---------- 未知键 ---------- */

// 默认**拒绝启动**：拼错一个键名不会有任何提示、值悄悄不生效 —— 这是所有配置错误里
// 最贵的一种，因为它伪装成"配置生效了"。
func TestUnknownKeyRejectsStartup(t *testing.T) {
	msg := panicOn(t, baseConfig+"crawler:\n  targt: https://example.com\n")
	for _, want := range []string{"未知配置键", "crawler.targt", "直接拒绝启动"} {
		if !strings.Contains(msg, want) {
			t.Fatalf("panic 信息里缺 %q，实得：\n%s", want, msg)
		}
	}
	// 不能再提示"有个开关可以降级" —— 那扇门在 v2.7.0 拆了
	if strings.Contains(msg, "strict_config") {
		t.Fatalf("不该再提降级开关，实得：\n%s", msg)
	}
}

// 升级后残留的旧键（server.monitor / crawler.target / browser.pool_size 那几次）
// 落在这里，没有"长得像"的键可建议 —— 那就只报键名，不硬凑。
func TestUnknownRemovedKeyHasNoBogusSuggestion(t *testing.T) {
	msg := panicOn(t, baseConfig+"server:\n  monitor: true\n")
	if !strings.Contains(msg, "server.monitor") {
		t.Fatalf("应报出旧键，实得：\n%s", msg)
	}
	if strings.Contains(msg, "是不是想写") {
		t.Fatalf("找不到相近的键时不该硬凑建议，实得：\n%s", msg)
	}
}

// batch_size 写 0 要**能用**：运行时本来就把 <= 0 当"默认 1000"（见 engine/batch.go 的 processInBatches），
// 而校验层以前按 "> 0" 拒收 —— 于是注释里那句"0 = 默认 1000"和实际行为对不上：照注释写反而起不来。
// 负数才是真错的（照原样传下去会进 LIMIT）。
func TestQueueBatchSizeZeroMeansDefault(t *testing.T) {
	cfg := loadString(t, baseConfig+
		"error_queue:\n  enabled: true\n  worker_count: 1\n  batch_size: 0\n")
	if cfg.ErrorQueue.BatchSize != 0 {
		t.Fatalf("batch_size: 0 应当被接受（运行时按默认 1000 走），实得 %d", cfg.ErrorQueue.BatchSize)
	}

	msg := panicOn(t, baseConfig+"error_queue:\n  enabled: true\n  worker_count: 1\n  batch_size: -1\n")
	if !strings.Contains(msg, "batch_size") {
		t.Fatalf("负数应当 panic 并点名 batch_size，实得：\n%s", msg)
	}
}

// 三个治理队列现在又是 config.yaml 里的全局段：合法的能正常解析，开了缺项才会 panic 点名。
func TestQueueSectionsParse(t *testing.T) {
	cfg := loadString(t, baseConfig+
		"error_queue:\n  enabled: true\n  worker_count: 1\n  interval: \"4h\"\n  max_retry: 3\n  batch_size: 100\n"+
		"recover_queue:\n  enabled: true\n  worker_count: 1\n  batch_size: 100\n"+
		"repeat_queue:\n  enabled: true\n  worker_count: 1\n  interval: \"2h\"\n  batch_size: 100\n")
	if !cfg.ErrorQueue.Enabled || cfg.ErrorQueue.WorkerCount != 1 || cfg.ErrorQueue.MaxRetry != 3 || cfg.ErrorQueue.BatchSize != 100 {
		t.Fatalf("error_queue 没解析对：%+v", cfg.ErrorQueue)
	}
	if !cfg.RecoverQueue.Enabled || cfg.RecoverQueue.WorkerCount != 1 || cfg.RecoverQueue.BatchSize != 100 {
		t.Fatalf("recover_queue 没解析对：%+v", cfg.RecoverQueue)
	}
	if !cfg.RepeatQueue.Enabled || cfg.RepeatQueue.WorkerCount != 1 || cfg.RepeatQueue.BatchSize != 100 || cfg.RepeatQueue.Interval <= 0 {
		t.Fatalf("repeat_queue 没解析对：%+v", cfg.RepeatQueue)
	}

	// 开了却缺 worker_count → panic 点名
	msg := panicOn(t, baseConfig+"error_queue:\n  enabled: true\n  interval: \"4h\"\n")
	if !strings.Contains(msg, "error_queue") || !strings.Contains(msg, "worker_count") {
		t.Fatalf("开了缺 worker_count 应当 panic 点名，实得：\n%s", msg)
	}
}

// 降级开关在 **v2.7.0 拆掉了**（过渡期结束）。原来那句 `app.strict_config: false`
// 现在自己就是一个**未知键** —— 想拿它关掉严格模式，只会更早地撞上严格模式本身。
// 这条用例把"门没了"钉住：不靠文档说，而是它真的会 panic。
func TestStrictConfigEscapeHatchIsGone(t *testing.T) {
	msg := panicOn(t, baseConfig+"app:\n  strict_config: false\n")
	if !strings.Contains(msg, "app.strict_config") {
		t.Fatalf("旧开关本身应当被报成未知键，实得：\n%s", msg)
	}
}

/* ---------- 表本身的不变量 ---------- */

// 每条规则的 key 都必须能在结构体里找到。防的是"表里写了一个不存在的键路径" ——
// 那种规则永远不会触发，而报错信息会指向一个假键，比不写还坏。
func TestRuleKeysExistInStruct(t *testing.T) {
	valid := validKeys()
	for _, r := range rules {
		// 表里用 * 表示"map 的值"，结构体反射出来是 []，规范化后再比
		want := strings.ReplaceAll(r.key, ".*.", "[].")
		if !slices.Contains(valid, want) {
			t.Errorf("规则键 %q（规范化后 %q）在 Config 结构体里找不到", r.key, want)
		}
	}
}

// 合法键集合是给"是不是想写 X"用的，由结构体反射得出。这条钉住几个容易漏的形状：
// map 字段（写成 []）、没有 mapstructure tag 的结构体字段（要当叶子收进来）。
func TestValidKeysShapes(t *testing.T) {
	got := validKeys()
	for _, want := range []string{
		"app.env",
		"log.dir",
		"crawler.breaker.window",
		"browser.headers[]", // map 的叶子
		"server.port",
		"db.log_level",
	} {
		if !slices.Contains(got, want) {
			t.Errorf("validKeys() 缺 %q", want)
		}
	}
	// 段本身不该作为可写键出现（写 crawler: {} 没有意义）
	if slices.Contains(got, "crawler") {
		t.Error("validKeys() 不该把配置段本身当成可写键")
	}
}

func TestEditDistance(t *testing.T) {
	for _, c := range []struct {
		a, b string
		want int
	}{
		{"", "", 0},
		{"abc", "abc", 0},
		{"wroker_count", "worker_count", 2},
		{"targt", "target", 1},
		{"abc", "", 3},
	} {
		if got := editDistance(c.a, c.b); got != c.want {
			t.Errorf("editDistance(%q, %q) = %d, want %d", c.a, c.b, got, c.want)
		}
	}
}
