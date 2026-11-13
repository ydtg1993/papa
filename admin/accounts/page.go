package accounts

import "github.com/ydtg1993/papa/v4/admin/page"

func Page() page.Descriptor {
	return page.Descriptor{Key: "accounts", Label: "账号管理", Section: "data", Script: Script}
}
