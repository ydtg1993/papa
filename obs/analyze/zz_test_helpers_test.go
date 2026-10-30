package analyze

import (
	"strings"

	"github.com/ydtg1993/papa/v3/obs"
)

// tableText 把一节的表格压成文本，方便断言"某一行/某个键出现在输出里"。
func tableText(res *obs.SectionResult) string {
	var b strings.Builder
	for _, tb := range res.Tables {
		b.WriteString(tb.Title + "\n")
		for _, row := range tb.Rows {
			b.WriteString(strings.Join(row, " | ") + "\n")
		}
	}
	return b.String()
}

// tableByTitle 按标题找表格：列名（例如「未完成（含排队）」）的断言从这里拿。
func tableByTitle(res *obs.SectionResult, title string) *obs.Table {
	for i := range res.Tables {
		if res.Tables[i].Title == title {
			return &res.Tables[i]
		}
	}
	return nil
}

// columnsContain 判断表格列名里有目标列。
func columnsContain(cols []string, want string) bool {
	for _, c := range cols {
		if c == want {
			return true
		}
	}
	return false
}
