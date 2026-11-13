package diagnostics

import "github.com/ydtg1993/papa/v4/admin/page"

func Page() page.Descriptor {
	return page.Descriptor{Key: "browser-diagnostics", Label: "浏览器诊断", Section: "system", Script: Script}
}
