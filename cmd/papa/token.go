package main

import (
	"fmt"
	"strings"

	"github.com/ydtg1993/papa/v2/internal/database"
	"github.com/ydtg1993/papa/v2/internal/tokenadmin"
	"github.com/ydtg1993/papa/v2/models"
)

// runTokenCmd 处理 `papa token ...`。
//
// 只做 `add`：生成一把后台访问令牌，库里只存 sha256，明文**只在这里打印一次**。
// 列表 / 新增 / 停用启用 / 删除都能在后台的「访问令牌」页上做 —— 这里保留是为了
// **全部令牌被停用时还能救回来**：那之后 /api/* 一律拒绝，页面上不去、也就建不了新的。
func runTokenCmd(args []string) error {
	usage := `usage: papa token add --operator <名字> [--note "备注"] [-c configs/config.yaml]

  --operator  令牌归属的人（必填）
  --note      备注，比如"运维机"
  -c/--config 配置文件路径，默认 configs/config.yaml（也可用 PAPA_CONFIG）`

	if len(args) == 0 || args[0] != "add" {
		return fmt.Errorf("usage: papa token add --operator <名字> [--note \"备注\"] [-c configs/config.yaml]")
	}

	var operator, note, cfgPath string
	rest := args[1:]
	for i := 0; i < len(rest); i++ {
		a := rest[i]
		take := func() (string, bool) {
			if i+1 >= len(rest) {
				return "", false
			}
			i++
			return rest[i], true
		}
		switch {
		case a == "--operator" || a == "-operator":
			v, ok := take()
			if !ok {
				return fmt.Errorf("--operator 缺少值\n\n%s", usage)
			}
			operator = v
		case strings.HasPrefix(a, "--operator="):
			operator = strings.TrimPrefix(a, "--operator=")
		case a == "--note" || a == "-note":
			v, ok := take()
			if !ok {
				return fmt.Errorf("--note 缺少值\n\n%s", usage)
			}
			note = v
		case strings.HasPrefix(a, "--note="):
			note = strings.TrimPrefix(a, "--note=")
		case a == "-c" || a == "--config":
			v, ok := take()
			if !ok {
				return fmt.Errorf("-c 缺少值\n\n%s", usage)
			}
			cfgPath = v
		case strings.HasPrefix(a, "--config="):
			cfgPath = strings.TrimPrefix(a, "--config=")
		default:
			return fmt.Errorf("未知参数 %q\n\n%s", a, usage)
		}
	}
	if strings.TrimSpace(operator) == "" {
		return fmt.Errorf("--operator 不能为空\n\n%s", usage)
	}

	cfg, ok := loadOptionalConfig(cfgPath)
	if !ok {
		return fmt.Errorf("读不到配置（用 -c 指定 configs/config.yaml，或设 PAPA_CONFIG）")
	}
	db, err := database.NewDB(cfg.DB)
	if err != nil {
		return fmt.Errorf("连接数据库失败: %w", err)
	}
	// 首次部署时令牌表可能还没建；AutoMigrate 是增量的，顺手建一下省一轮手工迁移
	if err := database.AutoMigrate(db, &models.AccessToken{}); err != nil {
		return fmt.Errorf("建表 crawler_access_token 失败: %w", err)
	}

	token, _, err := tokenadmin.NewStore(db).Create(operator, note)
	if err != nil {
		return fmt.Errorf("写入令牌失败: %w", err)
	}

	fmt.Printf("已为「%s」创建访问令牌（**只显示这一次**，请立刻存好）：\n\n  %s\n\n", strings.TrimSpace(operator), token)
	fmt.Println("登录后台时把它填进访问令牌输入框；要停用/删除去后台的「访问令牌」页（在「设置」上方）。")
	return nil
}
