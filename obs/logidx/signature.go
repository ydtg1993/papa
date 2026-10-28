package logidx

import (
	"regexp"
	"strings"
)

// 签名的掩码常量与上限。
const (
	patternLimit     = 120
	placeholderURL   = "<url>"
	placeholderID    = "<id>"
	placeholderDigit = "#"
)

// 掩码正则。顺序有意义：先吞掉整条 URL，再处理长 hex/UUID，最后才把剩下的数字串换成 #，
// 否则 URL/UUID 内部的数字会先被打散，签名就对不上了。
var (
	// URL 只吃可打印 ASCII：日志里 URL 常与中文紧挨着（"访问 https://x 失败"），
	// 用 \S+ 会把后面的中文也吞进 URL。
	urlPattern    = regexp.MustCompile(`(?i)https?://[!-~]+`)
	uuidPattern   = regexp.MustCompile(`\b[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}\b`)
	hexPattern    = regexp.MustCompile(`\b(?:0[xX])?[0-9a-fA-F]{8,}\b`)
	numberPattern = regexp.MustCompile(`[0-9]+`)
)

// urlTrailing URL 后面常跟的标点：掩码时不吃掉它们，否则 "url: 失败" 与 "url 失败"
// 会变成同一条签名。
const urlTrailing = `.,;:!?)]}>"'，。；：！？）】》`

// signatureOf 把一条 msg 归一成签名：URL -> <url>，长 hex/UUID -> <id>，数字串 -> #，
// 最后按字符截断到 patternLimit。
func signatureOf(msg string) string {
	s := urlPattern.ReplaceAllStringFunc(msg, func(match string) string {
		trimmed := strings.TrimRight(match, urlTrailing)
		if trimmed == "" {
			return placeholderURL
		}
		return placeholderURL + match[len(trimmed):]
	})
	s = uuidPattern.ReplaceAllString(s, placeholderID)
	s = hexPattern.ReplaceAllStringFunc(s, func(match string) string {
		if looksLikeID(match) {
			return placeholderID
		}
		return match
	})
	s = numberPattern.ReplaceAllString(s, placeholderDigit)
	return truncateRunes(s, patternLimit)
}

// looksLikeID 判断 hex 形态的候选串是否真的当 ID 掩码：
// 带 0x/0X 前缀的一定是地址；否则要求至少含一个 a-f 字母 ——
// 纯数字长串应当走"数字 -> #"，而不是被当成 hex ID。
func looksLikeID(s string) bool {
	if len(s) >= 2 && s[0] == '0' && (s[1] == 'x' || s[1] == 'X') {
		return true
	}
	for i := 0; i < len(s); i++ {
		switch c := s[i]; {
		case c >= 'a' && c <= 'f', c >= 'A' && c <= 'F':
			return true
		}
	}
	return false
}

// truncateRunes 按字符（rune）截断，避免把中文截成半个字符。
func truncateRunes(s string, limit int) string {
	if limit <= 0 {
		return ""
	}
	count := 0
	for i := range s {
		if count == limit {
			return s[:i]
		}
		count++
	}
	return s
}
