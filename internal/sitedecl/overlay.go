package sitedecl

import (
	"strings"

	"github.com/ydtg1993/papa/v4/core"
	"github.com/ydtg1993/papa/v4/pkg/middleware/filedown"
	"github.com/ydtg1993/papa/v4/pkg/middleware/m3u8"
)

// DescribeDownloadOverlays 列出各站点/阶段**改过哪些下载器配置**（没写的层不出现在结果里）。
//
// 为什么要有：下载器有全局/站点/阶段三层声明 + fetcher 里还能随手 `.With()` 造临时实例 ——
// 分层的代价就是"改一处要看几份声明"。`papa check` 把生效的那些项打出来，是唯一便宜的补偿
// （真起服务时启动日志也会落一条同样的）。纯函数，不连库、不建实例。
func DescribeDownloadOverlays(sites ...SiteSpec) []string {
	var out []string
	for _, spec := range sites {
		name := spec.Key
		if line := overlayLine("站点 "+name, spec.M3U8, spec.FileDown); line != "" {
			out = append(out, line)
		}
		for _, st := range spec.Stages {
			if st.Fetcher == nil {
				continue
			}
			if line := overlayLine("阶段 "+core.StageKey(spec.Key, st.Fetcher.GetStage()), st.M3U8, st.FileDown); line != "" {
				out = append(out, line)
			}
		}
	}
	return out
}

// overlayLine 把一层的覆盖项拼成一行；两项都没写就返回空串（调用方据此跳过）。
func overlayLine(where string, m3u8Ov *m3u8.Overlay, fdOv *filedown.Overlay) string {
	var parts []string
	if m3u8Ov != nil {
		if fields := m3u8Ov.Fields(); len(fields) > 0 {
			parts = append(parts, "m3u8{"+strings.Join(fields, " ")+"}")
		}
	}
	if fdOv != nil {
		if fields := fdOv.Fields(); len(fields) > 0 {
			parts = append(parts, "filedown{"+strings.Join(fields, " ")+"}")
		}
	}
	if len(parts) == 0 {
		return ""
	}
	return where + ": " + strings.Join(parts, " ")
}
