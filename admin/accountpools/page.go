package accountpools

import "github.com/ydtg1993/papa/v4/admin/page"

func Page() page.Descriptor {
	return page.Descriptor{Key: "account-pools", Label: "账号池", Section: "data", Script: Script}
}
