package config

import (
	"fmt"
	"reflect"
	"sort"
	"strings"
	"time"
)

// ─────────────────────────────────────────────────────────────────────────────
// 配置校验层
//
// 这一层只管两件事，都作用在**配置文件**（config.yaml）上：
//
//  1. **键名**：文件里有、结构体里没有的键 —— **一律**拒绝启动，没有降级开关；
//  2. **值域**：已声明字段的合法范围 —— 越界直接 panic。
//
// 它**不管**各库自己的 Go 结构体零值（filedown.ChunkSize、htmlfetch.MaxBodySize 那类）——
// 那些是 API 误用，判据归各库构造函数自己，别把两个入口混成一层。
//
// 为什么要这一层：这一档的毛病全是「静默变成另一种行为」——漏写 log.dir 会去写盘根目录、
// 拼错一个键名会让值悄悄不生效、refresh_interval 配 0 会在某个 goroutine 里崩掉整个进程。
// 它们都不报错，只会让人往错的方向排查。把这些判据收成一张表，
// 比在每个 New* 里手写 if x <= 0 更难漏。
// ─────────────────────────────────────────────────────────────────────────────

// breakerMinWindow 熔断窗口的下限：窗口要切成 60 个等宽桶，桶宽 = window/60，
// 小于 60ns 时桶宽被整数除法算成 0，add / sumLocked 会整数除零。
const breakerMinWindow = 60 * time.Nanosecond

// Validate 校验一份配置。**不符合直接 panic。**
//
// 为什么是 panic 而不是返回 error：判据与 App.RegisterSites 对阶段声明那几行、
// internal/breaker.New 对 threshold 那条一致 —— 非法配置该在**启动时**炸掉，
// 而不是等跑到某条任务上才变成另一种行为。
//
// unused 传 mapstructure 的 Metadata.Unused，即「文件里有、结构体里没有」的键路径。
func Validate(c *Config, unused []string) {
	if err := checkUnusedKeys(unused); err != nil {
		panic(err)
	}
	// 先补默认值再跑规则：规则里的"没写"这一态（三态里的第一态）已经在这里归一成
	// 生效值，规则只需判"写了什么" —— 于是"没写"和"写了零值"两条路走同一个结果。
	applyDefaults(c)
	for _, r := range rules {
		if err := r.check(c); err != nil {
			panic(fmt.Errorf("config: %s\n%s", r.key, err))
		}
	}
}

// 观测与日志配置的默认值。
//
// 在 Validate 里补（而不是在 Load 解码后、或各消费方各写一个 OrDefault）：这里是
// **配置生效值的唯一出口**，消费方（internal/app 等）读到的永远是生效值，
// 不用再记住"空串要当哪个默认值"。
const (
	defaultLogLevel            = "info"
	defaultObsEventsFile       = "events.jsonl"
	defaultObsEventsBuffer     = 8192
	defaultObsReportDir        = "./reports"
	defaultObsReportRetention  = 168 * time.Hour // 7 天：报告产物按天拆分后默认只留一周
	defaultObsReportTimeout    = 120 * time.Second
	defaultObsStuckAfter       = 10 * time.Minute
	defaultObsCrawlFailRateMax = 0.05
	// defaultRecoverBatchPause 启动恢复的默认批间停顿：把一次恢复摊到多个时间片里。
	// 30m 是"第一批投完先让 worker 消化，再投下一批"的粗粒度节拍；可在 recover_queue.batch_pause 覆盖。
	defaultRecoverBatchPause = 30 * time.Minute
)

// validLogLevels 是 log.level 的合法取值，切片顺序即报错信息里的展示顺序。
// warning 是 warn 的别名（logrus 只有一档 WarnLevel）。
var validLogLevels = []string{"debug", "info", "warn", "warning", "error"}

// applyDefaults 把**可选键**的"没写 / 零值"归一成生效值。
//
// 只碰可选键：必填键（log.dir、db 的连接池两键）仍由规则拒绝，不会被这里"补"成合法。
// 空白串与没写等价 —— 与 log.dir / archive.dir 的判法一致（YAML 里写了个空值也是没写）。
func applyDefaults(c *Config) {
	if strings.TrimSpace(c.Log.Level) == "" {
		c.Log.Level = defaultLogLevel
	}
	if strings.TrimSpace(c.Obs.Events.File) == "" {
		c.Obs.Events.File = defaultObsEventsFile
	}
	if c.Obs.Events.Buffer <= 0 {
		c.Obs.Events.Buffer = defaultObsEventsBuffer
	}
	// 0 与负数都是"不发"：负间隔不能交给 time.NewTicker（它会 panic）。
	if c.Obs.Events.SnapshotInterval < 0 {
		c.Obs.Events.SnapshotInterval = 0
	}
	// 启动恢复的批间停顿：没写（0）补默认 30m；要连续跑完请显式写很小的值（如 1ms）。
	// 写负数没有语义，归一成 0 后同样补默认值 —— 避免 time.NewTimer(负数) 在后台协程里 panic。
	if c.RecoverQueue.BatchPause < 0 {
		c.RecoverQueue.BatchPause = 0
	}
	if c.RecoverQueue.BatchPause == 0 {
		c.RecoverQueue.BatchPause = defaultRecoverBatchPause
	}
	if strings.TrimSpace(c.Obs.Report.Dir) == "" {
		c.Obs.Report.Dir = defaultObsReportDir
	}
	// 报告保留期：0/未写补默认 7 天；负数 = 永久保留（与 crawler.trace.retention 同一口径，刻意保留）。
	if c.Obs.Report.Retention == 0 {
		c.Obs.Report.Retention = defaultObsReportRetention
	}
	if c.Obs.Report.Timeout <= 0 {
		c.Obs.Report.Timeout = defaultObsReportTimeout
	}
	if c.Obs.Report.Thresholds.StuckAfter <= 0 {
		c.Obs.Report.Thresholds.StuckAfter = defaultObsStuckAfter
	}
	if c.Obs.Report.Thresholds.CrawlFailRateMax <= 0 {
		c.Obs.Report.Thresholds.CrawlFailRateMax = defaultObsCrawlFailRateMax
	}
}

// validLogLevel 报告 log.level 是否合法。空串也算合法（"留空 = info"），
// 虽然 applyDefaults 之后不会再出现空串 —— 两条路都放行，免得将来调用顺序一变就炸。
func validLogLevel(name string) bool {
	if name == "" {
		return true
	}
	for _, lvl := range validLogLevels {
		if name == lvl {
			return true
		}
	}
	return false
}

// rule 一条值域规则。key 用**配置文件里的键路径**写法（与文档、与报错信息同一套），
// check 返回非 nil 即越界。
//
// 三态是这一层最容易做错的地方，每条规则都必须回答：
//   - **没写**（零值）→ 用默认值，**合法**；
//   - 写了且合法 → 放行；
//   - 写了但越界 → 拒绝。
//
// 漏掉第一态就会把一堆可选键变成必填，直接炸掉所有老配置。
type rule struct {
	key   string
	check func(*Config) error
}

var rules = []rule{
	{"db.log_level", func(c *Config) error {
		// 从 Load 的返回路径搬进来的：它本来就是一条值域规则，
		// 和别的放一起才不会被漏掉（代价是它从返回 error 变成了 panic）。
		if !validSQLLogLevel(c.DB.LogLevel) {
			return fmt.Errorf("  只能是 %s / %s / %s / %s（留空表示按 app.env 推），实得 %q",
				SQLLogSilent, SQLLogError, SQLLogWarn, SQLLogInfo, c.DB.LogLevel)
		}
		return nil
	}},

	{"log.level", func(c *Config) error {
		// 没写/写空已在 applyDefaults 里归一为 info（合法）；这里只判枚举。
		// 不在这里拒绝的话，未知级别会被 pkg/loggers 静默兜底成 info ——
		// 配了个名字却不生效，正是这一层要消灭的那类静默。
		if validLogLevel(c.Log.Level) {
			return nil
		}
		return fmt.Errorf("  只能是 %s（warning 是 warn 的别名；留空 = %s），实得 %q",
			strings.Join(validLogLevels, " / "), defaultLogLevel, c.Log.Level)
	}},

	{"crawler.breaker.window", func(c *Config) error {
		w := c.Crawler.Breaker.Window
		if w > 0 && w < breakerMinWindow {
			return fmt.Errorf("  窗口要切成 60 个等宽桶（桶宽 = window/60），小于 %s 时桶宽被整数除法算成 0，\n"+
				"  熔断计数会在 add / sumLocked 里整数除零。实得 %s；留空或 0 表示用默认 5m", breakerMinWindow, w)
		}
		return nil
	}},

	{"crawler.breaker.threshold", func(c *Config) error {
		b := c.Crawler.Breaker
		if b.Enabled && b.Threshold <= 0 {
			return fmt.Errorf("  启用熔断时必须 > 0（没写与写 <= 0 都算没给），实得 %d；\n"+
				"  要关掉熔断请把 enabled 改成 false", b.Threshold)
		}
		return nil
	}},

	{"proxy.refresh_interval", func(c *Config) error {
		if c.Proxy.APIURL == "" {
			return nil // 没配代理 API 就不会起刷新协程，这个值用不上
		}
		if c.Proxy.RefreshInterval <= 0 {
			return fmt.Errorf("  配了 proxy.api_url 就会起定时刷新，间隔必须 > 0（实得 %s）；\n"+
				"  为 0 时 time.NewTicker(0) 会在刷新协程里 panic，而那个 goroutine 没人 recover —— 整个进程会挂掉",
				c.Proxy.RefreshInterval)
		}
		return nil
	}},

	{"log.dir", func(c *Config) error {
		if strings.TrimSpace(c.Log.Dir) == "" {
			return fmt.Errorf("  日志目录不能为空 —— 空串会拼出「文件系统根 / 当前盘根」下的 sys.log：\n" +
				"  要么因为没权限而静默一条日志都没有，要么把日志散在盘根。给一个目录，如 ./logs")
		}
		return nil
	}},

	{"html.max_body_size", func(c *Config) error {
		if !c.HTML.Enable {
			return nil // 静态抓取关着时这个值用不上（htmlConfig 只在 enable 时才跑）
		}
		v := c.HTML.MaxBodySize
		if v < htmlMaxBodyMin || v > htmlMaxBodyMax {
			return fmt.Errorf("  必须在 %d..%d 字节之间（实得 %d）——\n"+
				"  下界挡的是**单位混淆**：隔壁 log.max_size 的单位是 MB，这里写 10 意思是 10 字节，\n"+
				"  于是每次抓取都报「页面太大」；上界 %d 在模板值（10MB）之上，只防 100MB 那种笔误",
				htmlMaxBodyMin, htmlMaxBodyMax, v, htmlMaxBodyMax)
		}
		return nil
	}},

	{"browser.pool_size", func(c *Config) error {
		if !c.Browser.Enable {
			return nil // 没启用浏览器池时这个值用不上
		}
		// 与 html.max_body_size 同一个形状：**开着的时候必填**。
		// 零值 0（漏写也一样）时池容量就是 0 —— get 里"复用空闲实例"与"未达上限则新建"
		// 两条路都跳过（各自带 `limit > 0` 的前缀），最后停在 cond.Wait 上**永久阻塞且不报错**；
		// 配上 CLI 那种 context.Background() 就是死等，连取消都等不到。
		// 隔壁 GetDirect 对同类情况是明确报错的（"direct browsers not configured"）——
		// 所以这个不对称是漏了校验，不是设计。
		if c.Browser.PoolSize <= 0 {
			return fmt.Errorf("  启用浏览器池时必须 > 0（实得 %d）——\n"+
				"  为 0（漏写也一样）时池容量是 0：Get 既不会复用空闲实例、也不会新建，\n"+
				"  最后停在 cond.Wait 上永久阻塞且不报错（GetDirect 对同类情况会明确报错）。\n"+
				"  要关掉浏览器池请把 enable 改成 false", c.Browser.PoolSize)
		}
		return nil
	}},

	{"browser.binary", func(c *Config) error {
		b := c.Browser
		switch b.Binary {
		case "":
			// 留空是默认态：给了 browser_path 就按 path，没给就按 rod —— 两种都合法
			return nil
		case BrowserBinaryRod, BrowserBinarySystem:
			if b.BrowserPath != "" {
				return fmt.Errorf("  binary=%q 时不能再配 browser_path（实得 %q）——\n"+
					"  这一项已经决定了二进制从哪来：rod = Rod 自动下载并管理，system = 系统已安装的那个；\n"+
					"  要指定某个可执行文件请写 binary: path", b.Binary, b.BrowserPath)
			}
		case BrowserBinaryPath:
			if strings.TrimSpace(b.BrowserPath) == "" {
				return fmt.Errorf("  binary: path 时必须同时给 browser_path ——\n" +
					"  没有路径就不知道启动哪个可执行文件（要交回 Rod 管理请写 binary: rod，或删掉 binary 留空）")
			}
		default:
			return fmt.Errorf("  只能是 %q（默认：Rod 自动下载和管理）/ %q（系统已安装的 Chrome / Chromium / Edge）/ %q（用 browser_path 指定），实得 %q",
				BrowserBinaryRod, BrowserBinarySystem, BrowserBinaryPath, b.Binary)
		}
		return nil
	}},

	// 键路径写作 flags[]：规则对象是 map 里的每个条目（validKeys 对 map 叶子就是这个形状）。
	{"browser.flags[]", func(c *Config) error {
		if len(c.Browser.Flags) == 0 {
			return nil
		}
		// 按键名排序再校验：map 遍历顺序随机，多个坏键时同一条配置每次报出来的
		// 那个键都不一样 —— 与 checkUnusedKeys 的排序同一个理由。
		names := make([]string, 0, len(c.Browser.Flags))
		for name := range c.Browser.Flags {
			names = append(names, name)
		}
		sort.Strings(names)
		for _, name := range names {
			if strings.TrimSpace(name) == "" {
				return fmt.Errorf("  开关名不能为空 —— 键就是 Chromium 的开关名（如 disable-gpu）")
			}
			if strings.Contains(name, "=") {
				return fmt.Errorf("  开关名 %q 里不能有 '=' ——\n"+
					"  键写开关名（如 blink-settings），值写它对应的值（如 \"imagesEnabled=false\"）；\n"+
					"  rod 会自己拼成 --%s=<值>", name, name)
			}
			if strings.HasPrefix(name, "rod-") {
				return fmt.Errorf("  开关名 %q 是 Rod 的内部参数，不能从 browser.flags 配 ——\n"+
					"  它们不走 Chromium 命令行（launcher 会跳过），只会静默改掉框架自己的启动行为", name)
			}
		}
		return nil
	}},

	{"db.max_idle_conns", func(c *Config) error {
		return inPoolRange(c.DB.MaxIdleConns)
	}},

	{"db.max_open_conns", func(c *Config) error {
		return inPoolRange(c.DB.MaxOpenConns)
	}},

	// 跨字段：idle > open 时 Go 会把 idle **静默压到 open**（sql.DB.SetMaxIdleConns 的文档就这么写的），
	// 于是配置里那个数是个谎话 —— 说了不数，却没有任何提示。与"漏写变成不限"同一类静默。
	// 键名用 max_idle_conns：panic 会打成「config: db.max_idle_conns / 不能大于 …」，
	// 指向的正是那个会被无声改写的字段。这一条排在上面两条之后，值本身越界时先报值域。
	{"db.max_idle_conns", func(c *Config) error {
		if c.DB.MaxIdleConns > c.DB.MaxOpenConns {
			return fmt.Errorf("  不能大于 db.max_open_conns（%d > %d）——\n"+
				"  Go 会把它静默压到 max_open_conns，你写的这个数不会生效",
				c.DB.MaxIdleConns, c.DB.MaxOpenConns)
		}
		return nil
	}},

	{"crawler.archive.dir", func(c *Config) error {
		if !c.Crawler.Archive.Enabled {
			return nil // 没开归档就用不上这个目录
		}
		if strings.TrimSpace(c.Crawler.Archive.Dir) == "" {
			return fmt.Errorf("  开了归档就必须给目录（archive.dir）——\n" +
				"  空串会拼成「文件系统根 / 当前盘根」下的 {stage}/…：要么没权限、一个文件也写不出来，\n" +
				"  要么把归档散在盘根。给一个目录，如 ./logs/fetcher-html")
		}
		return nil
	}},

	{"crawler.archive.mode", func(c *Config) error {
		m := c.Crawler.Archive.Mode
		if m == "" || m == ArchiveModeFailure || m == ArchiveModeAlways {
			return nil // 留空 = failure
		}
		return fmt.Errorf("  只能是 %q（默认：只在失败的尝试落盘）或 %q（每次尝试都落，排查期用），实得 %q",
			ArchiveModeFailure, ArchiveModeAlways, m)
	}},

	{"crawler.archive.max_file_mb", func(c *Config) error {
		v := c.Crawler.Archive.MaxFileMB
		if v == 0 {
			return nil // 未写 = 默认 8MB
		}
		if v < archiveMaxFileMinMB || v > archiveMaxFileMaxMB {
			return fmt.Errorf("  必须在 %d..%d 之间（实得 %d）——\n"+
				"  这一项的单位是 **MB**（不是字节）：写 8 是「8MB 以内才归档」，而 1 就已经比"+
				"绝大多数页面大了；上界 %d 只挡把字节数写进来的那种笔误",
				archiveMaxFileMinMB, archiveMaxFileMaxMB, v, archiveMaxFileMaxMB)
		}
		return nil
	}},

	{"error_queue.worker_count", func(c *Config) error {
		if c.ErrorQueue.Enabled && c.ErrorQueue.WorkerCount <= 0 {
			return fmt.Errorf("  开启时必须 > 0（实得 %d）—— 为 0 时池子里没有 worker，失败任务会静静躺在队列里", c.ErrorQueue.WorkerCount)
		}
		return nil
	}},

	{"error_queue.batch_size", func(c *Config) error {
		if c.ErrorQueue.Enabled && c.ErrorQueue.BatchSize < 0 {
			return fmt.Errorf("  不能为负（实得 %d）—— 每批处理多少条；0 = 用默认的 1000（分页流式，避免一次性全量加载）", c.ErrorQueue.BatchSize)
		}
		return nil
	}},

	{"error_queue.max_retry", func(c *Config) error {
		if c.ErrorQueue.Enabled && c.ErrorQueue.MaxRetry < 0 {
			return fmt.Errorf("  不能为负（实得 %d）—— 0 = 不限代数", c.ErrorQueue.MaxRetry)
		}
		return nil
	}},

	{"error_queue.interval", func(c *Config) error {
		if c.ErrorQueue.Enabled && c.ErrorQueue.Interval < 0 {
			return fmt.Errorf("  interval 不能为负（实得 %s）—— 0 = 不自动轮询，仅后台手动触发", c.ErrorQueue.Interval)
		}
		return nil
	}},

	{"recover_queue.worker_count", func(c *Config) error {
		if c.RecoverQueue.Enabled && c.RecoverQueue.WorkerCount <= 0 {
			return fmt.Errorf("  开启时必须 > 0（实得 %d）—— 为 0 时池子里没有 worker，恢复出来的任务会静静躺在队列里", c.RecoverQueue.WorkerCount)
		}
		return nil
	}},

	{"recover_queue.batch_size", func(c *Config) error {
		if c.RecoverQueue.Enabled && c.RecoverQueue.BatchSize < 0 {
			return fmt.Errorf("  不能为负（实得 %d）—— 每批处理多少条；0 = 用默认的 1000", c.RecoverQueue.BatchSize)
		}
		return nil
	}},

	{"recover_queue.batch_pause", func(c *Config) error {
		// 默认值已在 applyDefaults 补齐（30m）；这里只拒绝"配了但归一不成合法时长"的负值。
		if c.RecoverQueue.BatchPause < 0 {
			return fmt.Errorf("  不能为负（实得 %s）—— 要连续跑完请写很小的正数（如 1ms），默认 30m", c.RecoverQueue.BatchPause)
		}
		return nil
	}},

	{"obs.report.interval", func(c *Config) error {
		// 0/不写 = 不自动生成；负数没有语义，直接拒绝（避免 time.NewTicker panic 被吞成启动崩溃）。
		if c.Obs.Report.Interval < 0 {
			return fmt.Errorf("  不能为负（实得 %s）—— 0/不写 = 不自动生成报告", c.Obs.Report.Interval)
		}
		return nil
	}},

	{"repeat_queue.worker_count", func(c *Config) error {
		if c.RepeatQueue.Enabled && c.RepeatQueue.WorkerCount <= 0 {
			return fmt.Errorf("  开启时必须 > 0（实得 %d）—— 为 0 时池子里没有 worker，到点的任务会静静躺在队列里", c.RepeatQueue.WorkerCount)
		}
		return nil
	}},

	{"repeat_queue.batch_size", func(c *Config) error {
		if c.RepeatQueue.Enabled && c.RepeatQueue.BatchSize < 0 {
			return fmt.Errorf("  不能为负（实得 %d）—— 每批处理多少条；0 = 用默认的 1000", c.RepeatQueue.BatchSize)
		}
		return nil
	}},

	{"repeat_queue.interval", func(c *Config) error {
		if c.RepeatQueue.Enabled && c.RepeatQueue.Interval <= 0 {
			return fmt.Errorf("  interval 必须 > 0（实得 %s）—— 这是「最粗兜底」的扫描节拍", c.RepeatQueue.Interval)
		}
		return nil
	}},
}

// 连接池两键允许的区间。**下界 1 才是真正起作用的那半**：配置的零值 0 与
// database/sql 的「0 = 不限 / 0 = 不保留空闲连接」是同一个数，mapstructure 分不开
// （除非把字段改成 *int），所以"漏写就静默变成不限"这条毛病靠范围校验拦不住 ——
// 只能把这两键收成**必填**。上界 100 是政策值（模板给的就是 10 / 100），
// 业务要开更多得改这里。
// html.max_body_size 的允许区间（字节）。下界挡**单位混淆**（隔壁 log.max_size 的单位是 MB，
// 而这个是字节：写 10 想表达 10MB 会得到 10 字节）；上界放在模板值 10MB 之上，
// 只防"100MB 那种笔误"，不去限制业务能抓多大的页面。
const (
	htmlMaxBodyMin int64 = 100 * 1024       // 100KB
	htmlMaxBodyMax int64 = 64 * 1024 * 1024 // 64MB
)

const (
	dbPoolMin = 1
	dbPoolMax = 100
)

const (
	archiveMaxFileMinMB = 1
	archiveMaxFileMaxMB = 1024
)

// inPoolRange 报告连接池取值是否在允许区间内。
func inPoolRange(v int) error {
	if v < dbPoolMin || v > dbPoolMax {
		return fmt.Errorf("  必须在 %d..%d 之间（实得 %d）——\n"+
			"  这两键是**必填**：漏写与写 0 都算没给，而 database/sql 把 0 当成「不限」（或「不保留空闲连接」），\n"+
			"  配置的零值也是 0，两者分不开 —— 不拦的话漏配就是静默打满 MySQL",
			dbPoolMin, dbPoolMax, v)
	}
	return nil
}

// checkUnusedKeys 处理「文件里有、结构体里没有」的键：**一律拒绝启动**。
//
// 这是所有配置错误里最贵的一种 —— 拼错一个键名不会有任何提示、值悄悄不生效，
// 而它伪装成「配置生效了」。升级时被删掉的旧键（`server.monitor` / `crawler.target` /
// `browser.pool_size` 那几次，文档当时写的正是「留着也被静默忽略」）也落在这里。
//
// **没有降级开关**（v2.7.0 起）：过渡期曾经有过一个 `app.strict_config: false`
// 把这一类降成警告，但那个开关本身也有两个毛病 —— 它是配置里的一行，写了就长期留着；
// 而"拼错键名"恰好是没人会主动去开严格模式的错误类型。规则既然定死，
// 就不留一个能被忽略的旁路。
func checkUnusedKeys(unused []string) error {
	if len(unused) == 0 {
		return nil
	}
	// 排序后再报：unused 来自 mapstructure，而它是走 yaml 解出来的 map ——
	// 顺序本来就不稳定，多条未知键时同一条命令每次报出来的次序都不一样。
	sorted := append([]string(nil), unused...)
	sort.Strings(sorted)
	quoted := make([]string, 0, len(sorted))
	for _, k := range sorted {
		quoted = append(quoted, fmt.Sprintf("%q", k))
	}
	var b strings.Builder
	fmt.Fprintf(&b, "  未知配置键：%s", strings.Join(quoted, "、"))
	if hint := suggestKeys(sorted); hint != "" {
		b.WriteString("\n" + hint)
	}
	b.WriteString("\n  这些键会被静默忽略、写进去的值不生效 —— 所以直接拒绝启动。")
	b.WriteString("\n  拼错了就改对；是从旧版本残留的就删掉（框架没有「静默忽略未知键」这一档）。")
	return fmt.Errorf("config: 未知配置键\n%s", b.String())
}

// suggestKeys 给未知键找最接近的合法键（编辑距离 <= 2），治「拼错一个字母」。
// 合法键集合由 Config 结构体反射得出 —— 手抄一份必然漂移。
func suggestKeys(unused []string) string {
	valid := validKeys() // 已按字典序排好，下面的遍历顺序必须确定 —— 见下
	byLeaf := make(map[string]string, len(valid))
	leaves := make([]string, 0, len(valid))
	for _, v := range valid {
		leaf := leafOf(v)
		if _, dup := byLeaf[leaf]; !dup {
			byLeaf[leaf] = v
			leaves = append(leaves, leaf)
		}
	}
	var lines []string
	for _, bad := range unused {
		leaf := leafOf(bad)
		best, bestDist := "", 3 // 只认距离 <= 2
		// **遍历切片而不是 map**：这里是"取距离最小的那个"，而 map 的遍历顺序是随机的 ——
		// 两个候选距离相同时谁赢就随机（`crawler.d` 距离 `dsn` 与 `dir` 都是 2），
		// 于是同一条命令每次跑出来的提示都不一样，测试也可能偶发红。
		for _, cand := range leaves {
			if d := editDistance(leaf, cand); d < bestDist {
				best, bestDist = cand, d
			}
		}
		if best != "" {
			// 只显示叶子名：完整路径上面那行刚列过，重复一遍读起来像列了两个键
			lines = append(lines, fmt.Sprintf("  %s → 是不是想写 %q？", leaf, byLeaf[best]))
		}
	}
	if len(lines) == 0 {
		return ""
	}
	sort.Strings(lines)
	return strings.Join(lines, "\n")
}

// leafOf 取键路径的最后一段：business[covers].dr → dr。
func leafOf(path string) string {
	if i := strings.LastIndex(path, "."); i >= 0 {
		return path[i+1:]
	}
	return path
}

// validKeys 反射出 Config 里所有合法的键路径。map 字段写成 business[]（叶子 map 不再往下展开）
// 这种带 [] 的形式 —— 与 mapstructure 报出来的路径写法对齐。
//
// 没有 mapstructure tag 的结构体字段（比如 DurationRange）当作**叶子**处理，
// 否则它整棵子树都不会出现在集合里。
func validKeys() []string {
	var out []string
	var walk func(t reflect.Type, prefix string)
	walk = func(t reflect.Type, prefix string) {
		for i := 0; i < t.NumField(); i++ {
			f := t.Field(i)
			tag := strings.Split(f.Tag.Get("mapstructure"), ",")[0]
			if tag == "" || tag == "-" {
				continue
			}
			path := tag
			if prefix != "" {
				path = prefix + "." + tag
			}
			switch {
			case f.Type.Kind() == reflect.Map:
				// 现在框架里只有 map[string]any / map[string]string 这类"叶子 map"
				//（business、各 headers）—— 再往下走的键不该被当配置键管辖。
				out = append(out, path+"[]")
			case f.Type.Kind() == reflect.Struct && hasMapstructureTags(f.Type):
				walk(f.Type, path)
			default:
				out = append(out, path)
			}
		}
	}
	walk(reflect.TypeOf(Config{}), "")
	sort.Strings(out)
	return out
}

// hasMapstructureTags 报告一个结构体是否是「配置段」（有子键要展开）。
func hasMapstructureTags(t reflect.Type) bool {
	for i := 0; i < t.NumField(); i++ {
		if t.Field(i).Tag.Get("mapstructure") != "" {
			return true
		}
	}
	return false
}

// editDistance 标准 Levenshtein 距离（两个词的滚动数组版）。不引依赖，就为了一句提示。
func editDistance(a, b string) int {
	ra, rb := []rune(a), []rune(b)
	if len(ra) == 0 {
		return len(rb)
	}
	prev := make([]int, len(rb)+1)
	cur := make([]int, len(rb)+1)
	for j := range prev {
		prev[j] = j
	}
	for i := 1; i <= len(ra); i++ {
		cur[0] = i
		for j := 1; j <= len(rb); j++ {
			cost := 1
			if ra[i-1] == rb[j-1] {
				cost = 0
			}
			cur[j] = min(prev[j]+1, cur[j-1]+1, prev[j-1]+cost)
		}
		prev, cur = cur, prev
	}
	return prev[len(rb)]
}
