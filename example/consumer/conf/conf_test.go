package conf

import (
	"strings"
	"testing"

	"github.com/jupiter-eng/xone/xonetest"
)

func TestLoad_BadValuesFailAtStartup(t *testing.T) {
	// 这两项配错都不报错，只会让服务「正常地什么都不做」
	cases := []struct{ yml, field string }{
		{"MyApp:\n  Workers: 0\n", "Workers"},
		{"MyApp:\n  MessageTimeout: 0s\n", "MessageTimeout"},
	}
	for _, c := range cases {
		useConf(t, c.yml)
		err := Load()
		if err == nil || !strings.Contains(err.Error(), c.field) {
			t.Errorf("%s 配错应当让启动失败并点出字段名，got=%v", c.field, err)
		}
	}
}

func TestLoad_DefaultsPassValidation(t *testing.T) {
	useConf(t, "MyApp:\n  Topic: orders\n")
	if err := Load(); err != nil {
		t.Fatal(err)
	}
}

// useConf 把一份配置装进全局配置，并还原 conf
func useConf(t *testing.T, yml string) {
	t.Helper()
	old := conf
	t.Cleanup(func() { conf = old })
	conf = DefaultConfig()
	xonetest.UseConfigYAML(t, yml)
}
