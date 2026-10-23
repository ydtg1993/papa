package database

import (
	"sync"
	"testing"

	"github.com/ydtg1993/papa/v3/config"
	"gorm.io/gorm/schema"
)

// TestFrameworkModelColumnsHaveComment 守住“框架模型字段必须写 COMMENT”的约定：
// comment tag 会随 AutoMigrate 进库。两个带开关的表也一起打开检查。
func TestFrameworkModelColumnsHaveComment(t *testing.T) {
	cfg := &config.Config{}
	cfg.Server.OperationLog = true
	cfg.Crawler.Trace.Enabled = true
	namer := schema.NamingStrategy{}
	for _, model := range FrameworkModels(cfg) {
		parsed, err := schema.Parse(model.Value, &sync.Map{}, namer)
		if err != nil {
			t.Fatalf("parse model %s: %v", model.Name, err)
		}
		for _, field := range parsed.Fields {
			if field.DBName == "" || field.IgnoreMigration {
				continue
			}
			if field.Comment == "" {
				t.Errorf("%s.%s.%s 缺少 gorm comment tag", model.Name, parsed.Name, field.DBName)
			}
		}
	}
}
