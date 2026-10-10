package main

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ydtg1993/papa/v3/config"
)

// 脚手架是用户照抄的范本：文件清单、目录结构、配置能否被框架真读进来，
// 都必须在生成的这一刻就验证过 —— 少了哪一个，用户要到 `go build` / 启动才发现。
func TestRunNewScaffoldsCompleteProject(t *testing.T) {
	t.Chdir(t.TempDir())

	if err := runNew("demo", nil); err != nil {
		t.Fatalf("runNew = %v", err)
	}

	wantFiles := []string{
		"go.mod",
		".gitignore",
		"main.go",
		"Makefile",
		filepath.Join("configs", "config.yaml"),
		filepath.Join("fetcher", "fetch_catalog.go"),
		filepath.Join("models", "models.go"),
		filepath.Join("monitor", "register.go"),
		filepath.Join("monitor", "router.go"),
		filepath.Join("monitor", "middleware.go"),
		filepath.Join("monitor", "tables.go"),
		filepath.Join("monitor", "controller", "ping.go"),
		filepath.Join("monitor", "view", "view.go"),
		filepath.Join("docker", "Dockerfile"),
		filepath.Join("docker", "docker-compose.yml"),
		filepath.Join("logs", ".gitkeep"),
		filepath.Join("configs", "whitelist"),
	}
	for _, f := range wantFiles {
		if _, err := os.Stat(filepath.Join("demo", f)); err != nil {
			t.Errorf("脚手架缺少 %s: %v", f, err)
		}
	}

	// 模块名要写进 go.mod（`papa new my/crawler` 这种带路径的名字也得原样用）
	if got := modulePath(readFile(t, filepath.Join("demo", "go.mod"))); got != "demo" {
		t.Errorf("go.mod 的 module = %q, want demo", got)
	}

	// 模板变量确实被替换过：main.go 里不该再留着 {{.Module}} 之类的占位
	mainGo := readFile(t, filepath.Join("demo", "main.go"))
	if strings.Contains(mainGo, "{{") {
		t.Errorf("main.go 里还有未替换的模板占位：\n%s", mainGo)
	}
	if !strings.Contains(mainGo, "github.com/ydtg1993/papa/v3") {
		t.Errorf("main.go 没 import 框架包：\n%s", mainGo)
	}
	// 迁移入口必须接上：建表只有这一条路，脚手架不接用户就无从建表
	if !strings.Contains(mainGo, "-migrate") {
		t.Errorf("main.go 没接上 -migrate 入口：\n%s", mainGo)
	}

	// 使用手册要一起复制过去（用户和 Claude 都靠它写 fetcher / model）
	docs, err := os.ReadDir(filepath.Join("demo", "docs"))
	if err != nil {
		t.Fatalf("读 docs: %v", err)
	}
	if len(docs) < 5 {
		t.Errorf("docs 只复制了 %d 个文件", len(docs))
	}
	for _, d := range docs {
		if d.IsDir() || !strings.HasSuffix(d.Name(), ".md") {
			t.Errorf("docs 下出现了非 markdown 文件：%s", d.Name())
		}
	}

	// 白名单文件是 config.yaml 里 server.whitelist_file 引用的，内容必须能被当注释全部忽略
	wl := readFile(t, filepath.Join("demo", "configs", "whitelist"))
	for line := range strings.SplitSeq(wl, "\n") {
		line = strings.TrimSpace(line)
		if line != "" && !strings.HasPrefix(line, "#") {
			t.Errorf("whitelist 模板里有意外的条目 %q —— 它会真的生效", line)
		}
	}
}

// 生成的 config.yaml 必须能被框架自己解析：模板改坏了要在这里炸，而不是在用户的项目里。
func TestScaffoldedConfigLoads(t *testing.T) {
	t.Chdir(t.TempDir())
	if err := runNew("demo", nil); err != nil {
		t.Fatalf("runNew = %v", err)
	}

	cfg, err := config.Load(filepath.Join("demo", "configs", "config.yaml"))
	if err != nil {
		t.Fatalf("脚手架产出的配置框架读不了：%v", err)
	}

	// `.gitignore` 不能整目录忽略 configs/ —— configs/sites/<站名>.go 是站点与阶段声明（真代码），
	// 被忽略掉的话新工程的骨架进不了版本库。回归点：v3.0.0 把声明搬进 configs/ 之后，
	// 模板里的 `configs/` 那行就成了坑（实测生成物里它就那么写着）。
	gitignore := readFile(t, filepath.Join("demo", ".gitignore"))
	if !strings.Contains(gitignore, "configs/config.yaml") {
		t.Errorf(".gitignore 应当忽略 configs/config.yaml：%s", gitignore)
	}
	for _, line := range strings.Split(gitignore, "\n") {
		if strings.TrimSpace(line) == "configs/" {
			t.Errorf(".gitignore 不该整目录忽略 configs/（configs/sites/*.go 是代码）：%s", gitignore)
		}
	}

	// 阶段参数已搬进 Go 声明：配置里**不再有** crawler.stages 段（上面的 Load 成功就是证明 ——
	// 模板若还留着那段，未知配置键会直接 panic），站点与阶段改由 configs/sites/<站名>.go 声明。
	siteSpec := readFile(t, filepath.Join("demo", "configs", "sites", "demo.go"))
	if strings.Contains(siteSpec, "Entries: map[string]string") {
		t.Errorf("生成的 SiteSpec 不应包含入口表 Entries 字段：\n%s", siteSpec)
	}
	if !strings.Contains(siteSpec, "fetcher.FetchCatalog{}") {
		t.Errorf("configs/sites/demo.go 里应当列出 catalog 阶段")
	}
	// 站点文件自己在 init 里登记（框架收集），项目里没有需要手写的汇总清单
	if !strings.Contains(siteSpec, "papa.RegisterSite(") {
		t.Errorf("configs/sites/demo.go 应当在 init 里自登记")
	}
	if _, err := os.Stat(filepath.Join("demo", "configs", "sites", "sites.go")); err == nil {
		t.Errorf("不该再生成 sites.go 汇总清单（各站点文件自己登记）")
	}
	fetcher := readFile(t, filepath.Join("demo", "fetcher", "fetch_catalog.go"))
	if strings.Contains(fetcher, "site.Entries") {
		t.Errorf("生成的 fetcher 不应读取 site.Entries：\n%s", fetcher)
	}
	if !strings.Contains(fetcher, `"catalog"`) {
		t.Errorf("fetcher 模板的 GetStage() 与配置里的阶段名对不上：\n%s", fetcher)
	}
	// 请求头只写**一处**：站点声明。模板里再抄一份到 config.yaml 就是会静默失效的副本
	//（站点级同键覆盖全局层），而脚手架是用户照抄的范本 —— 两处各写一份 UA 正是要杜绝的样子。
	if !strings.Contains(siteSpec, "Headers: map[string]string{") {
		t.Errorf("configs/sites/demo.go 应当把站点的请求头声明出来（脚手架产物要能直接对着真站跑）：\n%s", siteSpec)
	}
	if !strings.Contains(siteSpec, "User-Agent") {
		t.Errorf("站点声明里应当给出 User-Agent（框架默认的 PapaStaticHTML/1.0 会被真实站点判为爬虫）：\n%s", siteSpec)
	}
	// 反面：config.yaml 里不该再出现一份 UA。全局那层仍在（注释里给了用法），
	// 它留给"与站点无关、所有站共用、还想热更"的头。
	if strings.Contains(readFile(t, filepath.Join("demo", "configs", "config.yaml")), "Mozilla/") {
		t.Error("config.yaml 里不该再写一份 UA —— 请求头写在 configs/sites/<站名>.go 的 Headers 里")
	}

	// 配置里引用的白名单文件得真的存在，否则那条注释形同虚设
	if cfg.Server.WhitelistFile == "" {
		t.Error("server.whitelist_file 没配")
	} else if _, err := os.Stat(filepath.Join("demo", cfg.Server.WhitelistFile)); err != nil {
		t.Errorf("配置引用的白名单文件不存在（%s）：%v", cfg.Server.WhitelistFile, err)
	}
}

// 项目名为空要报错，不能生成一堆名为 "" 的文件（那等于往当前目录里乱写）。
func TestRunNewRejectsEmptyName(t *testing.T) {
	t.Chdir(t.TempDir())
	if err := runNew("", nil); err == nil {
		t.Fatal("空项目名应当报错")
	}
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("报错时不该落任何文件，实得 %v", entries)
	}
}

// --replace 是本地验证用的：把依赖指到本地 papa 仓库，省掉先发布一次。
func TestRunNewAppendsReplaceDirective(t *testing.T) {
	t.Chdir(t.TempDir())
	// Windows 反斜杠路径要转成斜杠，否则 go.mod 里的 replace 不是合法路径
	if err := runNew("demo", []string{"--replace", `E:\repo\papa`}); err != nil {
		t.Fatalf("runNew = %v", err)
	}

	gomod := readFile(t, filepath.Join("demo", "go.mod"))
	if !strings.Contains(gomod, "replace github.com/ydtg1993/papa/v3 => E:/repo/papa") {
		t.Fatalf("go.mod 里没有正确的 replace 指令：\n%s", gomod)
	}

	// 也支持 --replace=<path> 写法
	t.Chdir(t.TempDir())
	if err := runNew("demo2", []string{"--replace=E:/repo/papa"}); err != nil {
		t.Fatalf("runNew = %v", err)
	}
	if got := readFile(t, filepath.Join("demo2", "go.mod")); !strings.Contains(got, "=> E:/repo/papa") {
		t.Fatalf("--replace= 写法没生效：\n%s", got)
	}
}

// 不带 --replace 时不该凭空多出 replace 行。
func TestRunNewWithoutReplaceLeavesGoModClean(t *testing.T) {
	t.Chdir(t.TempDir())
	if err := runNew("demo", nil); err != nil {
		t.Fatal(err)
	}
	if got := readFile(t, filepath.Join("demo", "go.mod")); strings.Contains(got, "replace ") {
		t.Fatalf("没传 --replace 却出现了 replace 行：\n%s", got)
	}
}

// 已经存在的目录里再生成一次：文件被覆盖而不是报错（重复跑脚手架应当幂等）。
func TestRunNewIsIdempotent(t *testing.T) {
	t.Chdir(t.TempDir())
	if err := runNew("demo", nil); err != nil {
		t.Fatal(err)
	}
	first := readFile(t, filepath.Join("demo", "main.go"))

	if err := runNew("demo", nil); err != nil {
		t.Fatalf("重复生成应当成功：%v", err)
	}
	if second := readFile(t, filepath.Join("demo", "main.go")); second != first {
		t.Fatal("重复生成的内容不一致")
	}
}

// usage 是给用户看的唯一提示：命令名和子命令都得列全，否则拼错了没人知道。
func TestUsageListsEverySubcommand(t *testing.T) {
	got := captureStderr(t, usage)
	for _, want := range []string{"papa new", "papa migrate", "papa token add", "papa html", "papa rod", "papa diff", "papa select"} {
		if !strings.Contains(got, want) {
			t.Errorf("usage 里缺 %q：\n%s", want, got)
		}
	}
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读 %s: %v", path, err)
	}
	return string(b)
}

// captureStderr 把 os.Stderr 换成管道，跑完 fn 后把写进去的内容取回来。
// （用法提示与"已落盘到 X"这类提示都走 stderr，好让 stdout 只留抓取结果。）
func captureStderr(t *testing.T, fn func()) string {
	t.Helper()
	old := os.Stderr
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe: %v", err)
	}
	os.Stderr = w
	defer func() { os.Stderr = old }()

	done := make(chan string, 1)
	go func() {
		b, _ := io.ReadAll(r)
		done <- string(b)
	}()

	fn()

	_ = w.Close()
	os.Stderr = old
	got := <-done
	_ = r.Close()
	return got
}
