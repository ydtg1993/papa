package app

import (
	"fmt"
	"github.com/ydtg1993/papa/v3/admin/server"
	"github.com/ydtg1993/papa/v3/pkg/rulequality"
)

func (a *App) qualityAdministration() server.QualityAdministration {
	if !a.Config.Quality.Enabled {
		return nil
	}
	store, err := rulequality.New(a.DB)
	if err != nil {
		panic(fmt.Errorf("质量监控初始化失败: %w", err))
	}
	return store
}
