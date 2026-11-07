// Package auth 负责后台的令牌校验与「操作人」身份传递。
//
// 之前是配置里的单个 auth_key、服务端做一次字符串比较（认不出人）；现在凭据是
// crawler_access_token 表里的多条令牌，每条属于一个操作人：
//   - 校验：把传来的令牌 sha256 后按唯一索引查表（且 enabled），命中则拿到操作人；
//   - 身份：中间件把操作人写进请求上下文，操作日志据此记「谁干的」。
//
// 令牌只存哈希，不存明文。
package auth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http"
	"strings"

	"github.com/ydtg1993/papa/v3/models"
	"gorm.io/gorm"
)

// operatorKey 请求上下文里存操作人的键（不导出，只能经 WithOperator/OperatorFrom 走）。
type operatorKey struct{}

// NewToken 生成一把新令牌：32 字节随机数的十六进制串（64 个字符）。
// 生成与哈希放在一起 —— 创建令牌的地方（CLI 与后台「访问令牌」页）都从这儿走。
func NewToken() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// Hash 返回令牌的 sha256 十六进制；入库与校验都用它。
func Hash(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

// verifiedQuery 校验用的查询（抽出来便于离线断言：查的是 hash，不是明文）。
func verifiedQuery(db *gorm.DB, token string) *gorm.DB {
	return db.Select("operator").
		Where("token_hash = ? AND enabled = ?", Hash(token), true)
}

// Verify 按令牌查表校验：命中且启用返回操作人。
// 查不到（令牌不存在 / 已停用）返回 ok=false，不算错误；DB 故障才返回 err。
func Verify(db *gorm.DB, token string) (operator string, ok bool, err error) {
	if db == nil || db.DryRun || token == "" {
		return "", false, nil
	}
	var t models.AccessToken
	err = verifiedQuery(db, token).Take(&t).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	if strings.TrimSpace(t.Operator) == "" {
		return "", false, nil
	}
	return t.Operator, true, nil
}

// CountEnabled 返回启用的令牌数；用于启动时提示是否存在可用凭据。
func CountEnabled(db *gorm.DB) (int64, error) {
	if db == nil {
		return 0, nil
	}
	var n int64
	err := db.Model(&models.AccessToken{}).Where("enabled = ?", true).Count(&n).Error
	return n, err
}

// CountAll 返回令牌总数（不论启用与否）。
func CountAll(db *gorm.DB) (int64, error) {
	if db == nil {
		return 0, nil
	}
	var n int64
	err := db.Model(&models.AccessToken{}).Count(&n).Error
	return n, err
}

// Required always requires login, including first deployment without tokens.
// Bootstrap using the local papa token add command; HTTP never bootstraps trust.
func Required(db *gorm.DB) bool {
	return NewTokenCounter(db, 0).Required()
}

// Logger 只要一个 Errorf —— 避免为一个日志接口把宿主拉进来。
type Logger interface {
	Errorf(format string, args ...any)
}

// Verifier extracts the credential and verifies its hash and enabled state on
// every request. Empty stores, nil DB and database errors never authorize.
func Verifier(db *gorm.DB, log Logger) func(r *http.Request) (string, bool) {
	return NewTokenCounter(db, 0).Verifier(log)
}

// Confirmer requires a valid token again for explicit destructive confirmation.
// Empty stores do not bypass this check. It is not a separate authorization role.
func Confirmer(db *gorm.DB, log Logger) func(token string) (operator string, ok bool) {
	return NewTokenCounter(db, 0).Confirmer(log)
}

// WarnIfNoToken 启动时调用：表里一条令牌都没有就喊一声。
// 这是首次部署的正常状态（不是配置写错），所以只警告不 panic；但必须显眼。
func WarnIfNoToken(db *gorm.DB, log Logger) {
	total, err := CountAll(db)
	if err != nil {
		if log != nil {
			log.Errorf("检查访问令牌失败: %s", err.Error())
		}
		return
	}
	if log == nil {
		return
	}
	enabled, err := CountEnabled(db)
	if err != nil {
		log.Errorf("检查访问令牌失败: %s", err.Error())
		return
	}
	switch {
	case total == 0:
		log.Errorf("警告：还没有任何访问令牌 —— 管理接口默认拒绝访问；请在服务器本机执行 papa token add --operator <名字>")
	case enabled == 0:
		log.Errorf("警告：%d 条访问令牌全部被停用 —— /api/* 现在对所有人拒绝；请执行 papa token add 补一条", total)
	}
}

// Extract 从请求里取令牌：`Authorization: Bearer <token>`，其次 `X-Auth-Key`。
//
// **刻意不看 `?key=`** —— URL 里的凭据会落进浏览器历史、代理与访问日志。
//
// 方案名按 **RFC 7235 做大小写不敏感**比较：`Bearer` / `bearer` / `BEARER` 都是合法的，
// 而这里原来用的是 `strings.HasPrefix(h, "Bearer ")` —— 客户端写小写会拿到一个
// 莫名其妙的 401（凭据明明是对的）。
//
// **令牌本身仍然是逐字节精确匹配**，不做大小写归一：它是密钥，不是标识符；
// 而且生成侧是 `hex.EncodeToString`，给出来的本来就是全小写 —— 没有"统一小写"这回事，
// 真要归一反而埋雷（哪天 `NewToken` 换成混合大小写，入库哈希与校验哈希就会对不上）。
func Extract(r *http.Request) string {
	if h := r.Header.Get("Authorization"); h != "" {
		// 只按空格切一次：方案名与凭据之间按规范是 1*SP
		if scheme, rest, ok := strings.Cut(h, " "); ok && strings.EqualFold(scheme, "bearer") {
			return strings.TrimSpace(rest)
		}
	}
	return strings.TrimSpace(r.Header.Get("X-Auth-Key"))
}

// WithOperator 把操作人放进请求上下文。
func WithOperator(ctx context.Context, operator string) context.Context {
	return context.WithValue(ctx, operatorKey{}, operator)
}

// OperatorFrom 取上下文里的操作人；没有（未鉴权 / 测试构造的请求）返回 ""。
func OperatorFrom(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	op, _ := ctx.Value(operatorKey{}).(string)
	return op
}
