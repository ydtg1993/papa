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
