package logidx

import (
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

// logfmt 里固定含义的三个键；其余键一律进 Entry.Fields。
const (
	fieldTime    = "time"
	fieldLevel   = "level"
	fieldMessage = "msg"
)

// 日志行的时间格式（logrus TextFormatter.TimestampFormat 的默认写法之一）。
const timestampLayout = "2006-01-02 15:04:05"

// rawLimit Entry.Raw 的字节上限：日志可能带很长的崩溃栈，证据字段没必要无限膨胀。
const rawLimit = 2048

// parseLine 把一行文本解析成 Entry。ok=false 表示这一行不可用：
// 不是合法 logfmt、缺 time/level、或 time 解析不出来。
//
// 只依赖 time 与 level 两个键：logrus 每条记录都有它们，而 msg 允许为空串。
func parseLine(line string, loc *time.Location, file, logger string, lineNo int) (Entry, bool) {
	fields, ok := parseLogfmt(line)
	if !ok {
		return Entry{}, false
	}
	rawTime, ok := fields[fieldTime]
	if !ok {
		return Entry{}, false
	}
	ts, err := time.ParseInLocation(timestampLayout, rawTime, loc)
	if err != nil {
		return Entry{}, false
	}
	level := fields[fieldLevel]
	if level == "" {
		return Entry{}, false
	}

	var extra map[string]string
	for k, v := range fields {
		switch k {
		case fieldTime, fieldLevel, fieldMessage:
			continue
		}
		if extra == nil {
			extra = make(map[string]string, len(fields))
		}
		extra[k] = v
	}

	return Entry{
		File:   file,
		Line:   lineNo,
		Time:   ts,
		Level:  level,
		Logger: logger,
		Msg:    fields[fieldMessage],
		Fields: extra,
		Raw:    truncateRaw(line),
	}, true
}

// parseLogfmt 解析单行 logfmt：key=value 以单个空格分隔，值可以带双引号。
//
// 双引号值的语法是 Go 字符串字面量（logrus 用 fmt 的 %q 写出来），所以直接交给
// strconv.Unquote：`\"`、`\n`、`\r`、`\t`、`\uXXXX` 等转义都会还原。
// 引号值内部的空格不会切断字段；不带引号的值以空格结束。
func parseLogfmt(s string) (map[string]string, bool) {
	out := make(map[string]string, 8)
	i, n := 0, len(s)
	for {
		for i < n && s[i] == ' ' {
			i++
		}
		if i >= n {
			return out, true
		}
		keyStart := i
		for i < n && s[i] != '=' && s[i] != ' ' {
			i++
		}
		if i >= n || s[i] != '=' || i == keyStart {
			return nil, false
		}
		key := s[keyStart:i]
		i++ // 跳过 '='

		if i < n && s[i] == '"' {
			quoted, next, ok := scanQuoted(s, i)
			if !ok {
				return nil, false
			}
			value, err := strconv.Unquote(quoted)
			if err != nil {
				return nil, false
			}
			out[key] = value
			i = next
			if i < n && s[i] != ' ' {
				return nil, false
			}
			continue
		}

		valueStart := i
		for i < n && s[i] != ' ' {
			i++
		}
		if i == valueStart {
			return nil, false // 空值只能写成 ""
		}
		out[key] = s[valueStart:i]
	}
}

// scanQuoted 从 start（必须是双引号）开始扫描一个完整的 Go 字符串字面量，
// 返回字面量原文与它后面第一个字节的下标。反斜杠后的任意字符都跳过，
// 这样 `\"` 不会被当成结束引号。
func scanQuoted(s string, start int) (literal string, next int, ok bool) {
	for i := start + 1; i < len(s); i++ {
		switch s[i] {
		case '\\':
			i++ // 跳过被转义的字符；越界时下面的循环条件会结束并判定失败
		case '"':
			return s[start : i+1], i + 1, true
		}
	}
	return "", 0, false
}

// truncateRaw 把原文截断到 rawLimit 字节，并退到 UTF-8 字符边界。
func truncateRaw(s string) string {
	if len(s) <= rawLimit {
		return s
	}
	cut := rawLimit
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut]
}

// loggerName 从文件名推出 logger 名：engine.log -> engine，engine.log.gz -> engine。
func loggerName(file string) string {
	name := file
	if strings.HasSuffix(strings.ToLower(name), ".gz") {
		name = name[:len(name)-len(".gz")]
	}
	if strings.HasSuffix(strings.ToLower(name), ".log") {
		name = name[:len(name)-len(".log")]
	}
	return name
}
