package engine

import (
	"bytes"
	"encoding/json"

	"github.com/ydtg1993/papa/v4/config"
	"github.com/ydtg1993/papa/v4/internal/database"
	"github.com/ydtg1993/papa/v4/pkg/events"
	"github.com/ydtg1993/papa/v4/pkg/loggers"
	"github.com/ydtg1993/papa/v4/pkg/middleware/filedown"
	"github.com/ydtg1993/papa/v4/pkg/middleware/m3u8"
	"github.com/ydtg1993/papa/v4/pkg/middleware/proxy"
	"gorm.io/gorm"
)

// GetDB 获取数据库操作实例
func (e *Engine) GetDB() *gorm.DB {
	return e.db
}

// Upsert 冲突时按 conflictColumns 更新 updateColumns，并回填主键，使 record 指向最终记录。
func (e *Engine) Upsert(record any, conflictColumns, updateColumns []string) error {
	return database.Upsert(e.db, record, conflictColumns, updateColumns)
}

// GetLoggerSet 获取日志管理器列表
func (e *Engine) GetLoggerSet() *loggers.LoggerSet {
	return e.loggerSet
}

// SetEventsEmitter 注入运行事件流（internal/app 组装好后调用）；传 nil 表示关闭。
// 必须在 Run/ApplyRegisterStage 之前调用：快照事件的 ticker 在那一刻才起。
func (e *Engine) SetEventsEmitter(em events.Emitter) {
	e.events = em
	e.breakers.events = em
}

// emit 发一条事件；emitter 为 nil（未开启）时是 no-op。
func (e *Engine) emit(ev events.Event) {
	if e.events == nil {
		return
	}
	e.events.Emit(ev)
}

// GetConfig 获取全局配置
func (e *Engine) GetConfig() *config.Config {
	return e.cfg
}

// marshalNoHTMLEscape 序列化要写进 JSON 列的值，**关掉 Go 默认的 HTML 转义**。
//
// 默认的 `json.Marshal` 把 `<` `>` `&` 写成 `<` `>` `&`，而业务存进这些列的
// 常常就是一段 HTML（页面片段、快照、富文本）—— 落在库里、后台表格里、SQL 客户端里
// 就是一堆 `<`，"存进去、肉眼扫一眼"这件事直接被劝退。
//
// 转义与否只是 JSON 的两种写法，`json.Unmarshal` 还原出的是同一个字符串 ——
// 所以改这个既不动老数据、也不影响读回（老行里的 `<` 照样读得出来）。
//
// 用 Encoder 而不是 Marshal：只有 Encoder 有 SetEscapeHTML。它会**多写一个换行**，去掉。
func marshalNoHTMLEscape(v any) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return bytes.TrimRight(buf.Bytes(), "\n"), nil
}

// SetProxy 设置代理。必须在 ApplyRegisterStage（App.RegisterSites 会走到）之前
// —— 池子/客户端在那一刻就把实例绑好了，之后再设换不回来。
func (e *Engine) SetProxy(proxy *proxy.Manager) {
	e.proxy = proxy
}

// GetProxy 获取代理管理器
func (e *Engine) GetProxy() *proxy.Manager {
	return e.proxy
}

// GetM3U8 获取 m3u8 下载器：**框架默认那份**（基座）。
// 按站点/阶段配过的那一份要用 `M3U8ForTask(task)` / `M3U8ForStage(stage)` 取。
func (e *Engine) GetM3U8() *m3u8.Downloader {
	return e.m3u8
}

// GetFiledown 获取文件下载器：**框架默认那份**（基座）。
// 按站点/阶段配过的那一份要用 `FiledownForTask(task)` / `FiledownForStage(stage)` 取。
func (e *Engine) GetFiledown() *filedown.Downloader {
	return e.filedown
}
