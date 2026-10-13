package breaker

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// 契约守卫：这个包**只做计数与配置校验**。
//
// 它曾经还持有暂停状态、还是实现 `workerpool.Gate` 的那道闸门（于是"人工暂停"与"自动止损"
// 共用一份状态，显示上互相打架）。现在暂停只有一处定义 —— `engine` 的暂停信号表；
// 这个包既不认识池子（workerpool），也不认识监控 DTO（core）。
//
// 一旦有人把暂停状态或 DTO 搬回来，这条会立刻红 —— 逼着改的人先想清楚该不该。
func TestBreakerStaysPure(t *testing.T) {
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("读包目录：%v", err)
	}
	forbidden := map[string]string{
		"github.com/ydtg1993/papa/v3/core":                "监控 DTO 属于 core；熔断器只该把失败数**返回**给引擎，由引擎去写暂停信号、发告警",
		"github.com/ydtg1993/papa/v3/internal/workerpool": "池子那侧只声明自己要的最小接口；熔断器不该反过来认识池子（闸门已搬到 engine 的暂停信号）",
	}
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(".", name))
		if err != nil {
			t.Fatalf("读 %s：%v", name, err)
		}
		src := string(raw)
		for imp, why := range forbidden {
			if strings.Contains(src, `"`+imp+`"`) {
				t.Errorf("%s import 了 %s —— %s", name, imp, why)
			}
		}
	}
}
