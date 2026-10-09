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
	for _, r := range rules {
		if err := r.check(c); err != nil {
			panic(fmt.Errorf("config: %s\n%s", r.key, err))
		}
	}
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

// ValidateRuntime 校验**运行期覆盖层**（`PUT /api/config` → `runtime.yaml` 那条入口）。
//
// 为什么单列一个入口：**这条路绕过 `config.Load`** —— `LoadRuntime` 只做 `yaml.Unmarshal`，
// `ApplyRuntimeConfig` 只做 Merge + Store，两边都没有校验。于是
// `PUT /api/config {"html":{"max_body_size":0}}` 能在不重启的情况下把每一次抓取都打坏，
// 而配置文件的校验完全看不见它（`htmlConfig` 从 `cfg` 与 `rt` 两处取值，都没有兜底）。
//
// 返回 error 而不是 panic：这条路上有人在等回应，报 400 比把服务打死有用 ——
// 与配置文件那条「启动即 panic」正好互补：一个只能死在启动，一个能当场报回去。
//
// 只覆盖 `RuntimeConfig` 里**有域**的字段。池大小 / 阶段配置那类不在这里（改需重启）。
func ValidateRuntime(rt *RuntimeConfig) error {
	if rt == nil {
		return nil
	}
	if v := rt.HTML.MaxBodySize; v != nil && (*v < htmlMaxBodyMin || *v > htmlMaxBodyMax) {
		return fmt.Errorf("html.max_body_size 必须在 %d..%d 字节之间（实得 %d）", htmlMaxBodyMin, htmlMaxBodyMax, *v)
	}
	// 用切片而不是 map：多条同时越界时，报哪一条必须是确定的（map 遍历顺序随机）
	for _, f := range []struct {
		name string
		v    *int
	}{
		{"error_queue.worker_count", rt.ErrorQueue.WorkerCount},
		{"error_queue.max_retry", rt.ErrorQueue.MaxRetry},
		{"error_queue.batch_size", rt.ErrorQueue.BatchSize},
		{"recover_queue.worker_count", rt.RecoverQueue.WorkerCount},
		{"recover_queue.batch_size", rt.RecoverQueue.BatchSize},
		{"repeat_queue.worker_count", rt.RepeatQueue.WorkerCount},
		{"repeat_queue.batch_size", rt.RepeatQueue.BatchSize},
	} {
		if f.v != nil && *f.v < 0 {
			return fmt.Errorf("%s 必须 >= 0（实得 %d）", f.name, *f.v)
		}
	}
	return nil
}
