package main

import (
	"os"
	"strings"
	"testing"
)

// `papa token` 只保留 add：全部令牌被停用时后台进不去，这条命令是唯一的救回路径。
// 参数解析必须在碰数据库之前把错说清楚 —— 用户往往是"后台进不去了"才来跑它。
func TestRunTokenCmdUsageErrors(t *testing.T) {
	cases := []struct {
		name string
		args []string
		want string
	}{
		{"没有子命令", nil, "usage: papa token add"},
		{"子命令拼错", []string{"list"}, "usage: papa token add"},
		{"--operator 缺值", []string{"add", "--operator"}, "--operator 缺少值"},
		{"--note 缺值", []string{"add", "--operator", "张三", "--note"}, "--note 缺少值"},
		{"-c 缺值", []string{"add", "--operator", "张三", "-c"}, "-c 缺少值"},
		{"未知参数", []string{"add", "--nope"}, "未知参数"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := runTokenCmd(c.args)
			if err == nil {
				t.Fatalf("runTokenCmd(%v) 应当报错", c.args)
			}
			if !strings.Contains(err.Error(), c.want) {
				t.Fatalf("错误信息 = %q，应当包含 %q", err.Error(), c.want)
			}
		})
	}
}

// 一串空白不等于"给了操作人"：库里 operator 有索引、审计按它查人，空值等于查不到是谁。
func TestRunTokenCmdRejectsBlankOperator(t *testing.T) {
	for _, c := range []struct {
		name string
		args []string
	}{
		{"纯空格", []string{"add", "--operator", "   "}},
		{"空串", []string{"add", "--operator="}},
		{"等号写法给空白", []string{"add", "--operator= \t "}},
	} {
		t.Run(c.name, func(t *testing.T) {
			err := runTokenCmd(c.args)
			if err == nil {
				t.Fatal("空操作人应当报错")
			}
			if !strings.Contains(err.Error(), "--operator 不能为空") {
				t.Fatalf("错误信息 = %q", err.Error())
			}
		})
	}
}

// 操作人合法、但配置读不到时：报"读不到配置"，而不是继续往下连库。
func TestRunTokenCmdNeedsConfig(t *testing.T) {
	t.Chdir(t.TempDir())
	t.Setenv("PAPA_CONFIG", "")

	err := runTokenCmd([]string{"add", "--operator", "运维机", "--note", "备用", "-c", "configs/nope.yaml"})
	if err == nil {
		t.Fatal("配置不存在时应当报错")
	}
	if !strings.Contains(err.Error(), "读不到配置") {
		t.Fatalf("错误信息 = %q，应当指向配置问题", err.Error())
	}
}

// 参数两种写法（`--flag 值` 与 `--flag=值`）都要认；这里用"读不到配置"当探针 ——
// 能走到那一步就说明参数已经被正确吃掉了。
func TestRunTokenCmdAcceptsBothFlagForms(t *testing.T) {
	t.Chdir(t.TempDir())
	t.Setenv("PAPA_CONFIG", "")

	forms := [][]string{
		{"add", "--operator", "张三", "--note", "机器A", "-c", "nope.yaml"},
		{"add", "--operator=张三", "--note=机器A", "--config=nope.yaml"},
		{"add", "-operator", "张三", "-note", "机器A", "--config", "nope.yaml"},
	}
	for _, args := range forms {
		err := runTokenCmd(args)
		if err == nil {
			t.Fatalf("%v：应当报错（配置不存在）", args)
		}
		if strings.Contains(err.Error(), "未知参数") || strings.Contains(err.Error(), "缺少值") {
			t.Fatalf("%v：参数没被认出来：%v", args, err)
		}
	}
}

// 用法里 `-` 与 `--` 前缀都写了，实际也得两种都认。
func TestRunTokenCmdUnknownFlagIsReported(t *testing.T) {
	err := runTokenCmd([]string{"add", "--operator", "张三", "-x", "y"})
	if err == nil || !strings.Contains(err.Error(), "未知参数") {
		t.Fatalf("err = %v", err)
	}
	if !strings.Contains(err.Error(), "usage") {
		t.Fatalf("报错时应当带上用法：%v", err)
	}
}

// 环境变量 PAPA_CONFIG 是文档里承诺的配置来源之一，得和 -c 一样被认。
func TestRunTokenCmdHonoursPAPACONFIGEnv(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	path := dir + string(os.PathSeparator) + "from-env.yaml"
	if err := os.WriteFile(path, []byte("db:\n  driver: sqlite\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PAPA_CONFIG", path)

	err := runTokenCmd([]string{"add", "--operator", "张三"})
	// 配置能读到 → 走到 NewDB → 驱动不支持，报的是数据库那条错，而不是"读不到配置"
	if err == nil {
		t.Fatal("不支持的驱动应当报错")
	}
	if strings.Contains(err.Error(), "读不到配置") {
		t.Fatalf("PAPA_CONFIG 没被认：%v", err)
	}
}
