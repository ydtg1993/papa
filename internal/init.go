package internal

import (
	"github.com/ydtg1993/papa/internal/config"
	"github.com/ydtg1993/papa/internal/loggers"
	"github.com/ydtg1993/papa/pkg/database"
)

func init() {
	// 1.初始化日志
	loggers.InitLogger()
	// 2. 加载配置
	err := config.Load("configs/config.yaml")
	if err != nil {
		loggers.SysLogger.Fatal("load config failed:", err)
	}
	// 3.加载数据库
	err = database.NewDB(&config.Cfg.DB)
	if err != nil {
		loggers.DBLogger.WithError(err).Fatal("connect database failed")
	}
}
