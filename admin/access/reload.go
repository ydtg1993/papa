package access

import (
	"github.com/ydtg1993/papa/v4/admin/configstore"
)

// LoadWhitelist 从配置存储读一次来源白名单（启动那一次与定期重载都走这里）。
//
// 生效语义（与 configstore.KeyIPWhitelist 的注释、docs 里那张表逐条对应）：
// **行不存在 / 空数组 / 值解析不出来 → 不限制来源**；数组里有效的那几条 → 只允许这些来源。
// 读失败单独记 Error —— 这一档等于把限制**悄悄放宽了**，日志里必须留下痕迹
// （语义是运营定的：宁可放宽，也不因为一次手改把自己锁在后台外面）。值的形状校验在 store 那层。
func LoadWhitelist(reader configstore.StringListReader, log Logger) []string {
	entries, err := reader.LoadStrings(configstore.KeyIPWhitelist)
	if err != nil {
		log.Errorf("来源白名单读不出来，按「不限制来源」继续：%s", err.Error())
		return nil
	}
	return entries
}

// WarnIfOpenToAll 生效白名单为空时喊一声。
//
// 空白名单是**合法**配置（脚手架生成的 whitelist 文件初始就只有一行注释），但必须说出来：
// 它和"配了却没生效"从行为上分不出来 —— 而后者正是"后台怎么谁都能打开"这类排查里
// 最难想到的方向。按 Error 级打，与 auth.WarnIfNoToken 一个路数（一个是来源那道门，
// 一个是凭据那道门；令牌缺失时管理接口仍拒绝访问）。
func WarnIfOpenToAll(log Logger, entries []string) {
	if len(entries) > 0 {
		return
	}
	log.Errorf("警告：生效的来源 IP 白名单为空 —— /monitor 与 /api/* 不限制**任何来源**" +
		"（管理接口仍强制校验访问令牌）；要限制来源，去后台设置页写 IP/CIDR（存在 crawler_config 表的 ip_whitelist 行）")
}
