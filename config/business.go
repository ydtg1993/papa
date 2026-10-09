package config

import (
	"fmt"

	"github.com/go-viper/mapstructure/v2"
)

// BusinessSection 把 `business.<key>` 那段解码到 out（out 需为指针）。
//
// business 段是**业务自己的配置**：框架不解析、不校验键名，原样透出（见 Config.Business）。
// 但"框架不校验"不等于"随便写" —— 自己解析时最想要的恰恰是框架那一层判据，所以这里都给上：
//
//   - **业务段里的未知键同样拒收**（ErrorUnused）：`business.covers.dr` 这种拼错会当场报错，
//     而不是留个零值让业务自己发现；
//   - 类型不对直接报错（mapstructure），不静默转换；
//   - 与框架的配置同一套解码 hook：`"5m"` → `time.Duration`、`"a,b"` → `[]string`。
//
// 需要完全自由的结构（键名由业务动态决定、或就是一段透传给别人的 JSON）时别用它，
// 直接读 `Config.Business` 那个 map。
func (c *Config) BusinessSection(key string, out any) error {
	raw, ok := c.Business[key]
	if !ok {
		return fmt.Errorf("config: business.%s 没配（业务段不做默认值：要用的段就显式写在 config.yaml 里）", key)
	}

	dec, err := mapstructure.NewDecoder(&mapstructure.DecoderConfig{
		Result:      out,
		ErrorUnused: true,
		// 与 Load 里那份保持一致：业务段里的时长/切片写法不该和框架的两套规矩。
		DecodeHook: mapstructure.ComposeDecodeHookFunc(
			mapstructure.TextUnmarshallerHookFunc(),
			mapstructure.StringToTimeDurationHookFunc(),
			mapstructure.StringToSliceHookFunc(","),
		),
	})
	if err != nil {
		return fmt.Errorf("config: business.%s: %w", key, err)
	}
	if err := dec.Decode(raw); err != nil {
		return fmt.Errorf("config: business.%s: %w", key, err)
	}
	return nil
}
