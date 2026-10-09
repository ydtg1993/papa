package htmlfetch

import (
	"errors"
	"fmt"
)

// 本文件是 htmlfetch 的**错误类型**：让业务判「该怎么处理这个失败」时有类型可用，
// 而不是去匹配错误文本。文本匹配在这里有真实的假阳性，见 StatusError 的注释。

// StatusError HTTP 响应码不是 2xx 时 Fetch 返回的错误。
//
// 业务判「404 / 410 这类重试也没用的失败」用它：
//
//	var se *htmlfetch.StatusError
//	if errors.As(err, &se) && se.Code == http.StatusNotFound {
//		return papa.WrapNoRetryKind("not-found", err)
//	}
//
// 也可以直接 `if code, ok := htmlfetch.StatusCode(err); ok && code == 404 { … }`。
//
// **别去匹配错误文本**：`strings.Contains(err.Error(), "404")` 会误伤 ——
// 把 `html.max_body_size` 配成 4040000 时，"html response exceeds 4040000 bytes"
// 里就带着 "404"，于是一个**该重试**的错误被判成"不可重试的 not_found"、一次就判死。
//
// 文案保持原样（"fetch html: unexpected status %d"）：老代码里可能有依赖它的匹配，
// 而这次改动只是**多给一条路**，不逼着谁改。
type StatusError struct {
	Code int
}

func (e *StatusError) Error() string {
	return fmt.Sprintf("fetch html: unexpected status %d", e.Code)
}

// BodyTooLargeError 响应体超过 `html.max_body_size` 时 Fetch 返回的错误。
//
// 与 StatusError 分成两个类型，是因为它们回答的是不同的问题：
// 状态码是**服务端的说法**（404 别重试、503 该重试），而这一条是**配置与页面体量撞出来的**，
// 同一个 URL 再抓一次通常还是这么大 —— 该不该重试由业务定，框架不替它判。
type BodyTooLargeError struct {
	MaxBodySize int64
}

func (e *BodyTooLargeError) Error() string {
	return fmt.Sprintf("html response exceeds %d bytes", e.MaxBodySize)
}

// StatusCode 取出错误链里的 HTTP 状态码；不是状态码错误时返回 (0, false)。
// 它是 `errors.As(err, &se)` 的省事写法，免得每处调用都写一遍指针那个 dance。
func StatusCode(err error) (int, bool) {
	var se *StatusError
	if errors.As(err, &se) {
		return se.Code, true
	}
	return 0, false
}
