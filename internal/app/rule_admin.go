package app

import (
	"fmt"
	"github.com/ydtg1993/papa/v3/admin/server"
	"github.com/ydtg1993/papa/v3/pkg/rulestore"
)

func (a *App) ruleAdministration() server.RuleAdministration {
	if !a.Config.Rules.Enabled {
		return nil
	}
	admin, err := rulestore.NewAdmin(a.DB)
	if err != nil {
		panic(fmt.Errorf("规则运维初始化失败: %w", err))
	}
	return admin
}
