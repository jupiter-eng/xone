package xginswagger

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/swaggo/swag"

	"github.com/jupiter-eng/xone/internal/hook"
	"github.com/jupiter-eng/xone/internal/testkit"
	"github.com/jupiter-eng/xone/xonetest"
)

func init() { gin.SetMode(gin.TestMode) }

// runStartHooks 走一遍各包自己的启动钩子：xapp 的配置是它自己读进去的
func runStartHooks(t *testing.T) {
	t.Helper()
	for _, e := range hook.Start() {
		if err := e.Run(context.Background()); err != nil {
			t.Fatalf("%s 失败：%v", e.Name, err)
		}
	}
}

func get(e *gin.Engine, path string) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	e.ServeHTTP(w, httptest.NewRequest("GET", path, nil))
	return w
}

func TestRegister_BeforeXoneRunInfoStillFromConfig(t *testing.T) {
	// xgin 允许在 xone.Run 之前就装配 engine（main 顶上调一下 Engine()）。
	// 配置那时读得到，标题默认取的 App.Name 却要等 xapp 的启动钩子——
	// 在 Register 那一刻就填的话，文档照样打得开，只是标题不对
	testkit.UseConfigEnv(t, "XApp:\n  Name: swagger.lazy.app\nXGinSwagger:\n  Host: api.example.com\n")
	info := testSpec()

	e := gin.New()
	Register(e, info) // 配置还没加载，xapp 也还没读
	runStartHooks(t)

	body := get(e, "/swagger/doc.json").Body.String()
	if !strings.Contains(body, "api.example.com") || !strings.Contains(body, "swagger.lazy.app") {
		t.Errorf("元信息该来自配置和 App，got=%s", body)
	}
}

// testSpec 登记到 swag 默认实例上的那份元信息。swag 不允许重复登记，
// 所以整个测试进程只登记一次，每次取用时把字段还原
var (
	specOnce sync.Once
	spec     = &swag.Spec{InfoInstanceName: swag.Name, SwaggerTemplate: `{"host":"{{.Host}}","title":"{{.Title}}"}`}
)

func testSpec() *swag.Spec {
	specOnce.Do(func() { swag.Register(swag.Name, spec) })
	spec.Title, spec.Host = "注解里的标题", "annotated"
	return spec
}

func TestRegister_BeforeXoneRunPrefixStillFromConfig(t *testing.T) {
	// 路由在注册那一刻就定死了。从前 Register 读的是启动钩子填的包变量，
	// 早于它就只能挂在默认前缀上、再让启动失败；现在配置什么时候读都是最终值
	testkit.UseConfigEnv(t, "XGinSwagger:\n  URLPrefix: /internal\n")

	e := gin.New()
	Register(e, nil)

	if w := get(e, "/internal/swagger/index.html"); w.Code != http.StatusOK {
		t.Errorf("应挂在配置的前缀下，got=%d", w.Code)
	}
	if !strings.HasPrefix(URL(), "/internal/") {
		t.Errorf("URL 应带上前缀，got=%s", URL())
	}
}

func TestLoadConfig_StartupFailsOnMalformedPrefix(t *testing.T) {
	// gin 自己会把它们规整成 /internal/swagger/*any，但 URL() 拼出来的就对不上了
	for _, p := range []string{"internal", "/internal/"} {
		xonetest.UseConfigYAML(t, "XGinSwagger:\n  URLPrefix: "+p+"\n")
		if err := loadConfig(context.Background()); err == nil {
			t.Errorf("URLPrefix=%q 该启动失败", p)
		}
	}
}

func TestRegister_MountsUI(t *testing.T) {
	xonetest.UseConfigYAML(t, "")
	e := gin.New()
	Register(e, nil)

	if w := get(e, "/swagger/index.html"); w.Code != http.StatusOK {
		t.Errorf("Swagger UI 应当可访问，got=%d", w.Code)
	}
}

func TestRegister_SupportsPrefix(t *testing.T) {
	xonetest.UseConfigYAML(t, "XGinSwagger:\n  URLPrefix: /internal\n")

	e := gin.New()
	Register(e, nil)

	if w := get(e, "/internal/swagger/index.html"); w.Code != http.StatusOK {
		t.Errorf("应挂在配置的前缀下，got=%d", w.Code)
	}
	if !strings.HasPrefix(URL(), "/internal/") {
		t.Errorf("URL 应带上前缀，got=%s", URL())
	}
}

func TestRegister_NilEngineDoesNotPanic(t *testing.T) {
	Register(nil, nil)
}

func TestFill_OverridesOnlyConfiguredFields(t *testing.T) {
	// 空的配置块不该把注解里写好的文档标题清成空白
	info := &swag.Spec{
		Title:       "注解里的标题",
		Description: "注解里的描述",
		BasePath:    "/v1",
		Host:        "annotated.example.com",
		Schemes:     []string{"http"},
	}
	fill(info, Config{})

	if info.Description != "注解里的描述" || info.BasePath != "/v1" || info.Host != "annotated.example.com" {
		t.Errorf("没配的字段应保留注解里的值，got=%+v", info)
	}
	if len(info.Schemes) != 1 || info.Schemes[0] != "http" {
		t.Errorf("没配 Schemes 就该保留注解里的，got=%v", info.Schemes)
	}
}

func TestFill_ConfiguredValueOverrides(t *testing.T) {
	info := &swag.Spec{Title: "注解里的标题", Host: "old"}
	fill(info, Config{
		Host: "api.example.com", BasePath: "/api/v2",
		Title: "配置里的标题", Description: "配置里的描述",
		Schemes: []string{"https"},
	})

	if info.Host != "api.example.com" || info.BasePath != "/api/v2" {
		t.Errorf("配了就该覆盖，got=%+v", info)
	}
	if info.Title != "配置里的标题" || info.Description != "配置里的描述" {
		t.Errorf("标题和描述应被覆盖，got=%+v", info)
	}
	if len(info.Schemes) != 1 || info.Schemes[0] != "https" {
		t.Errorf("Schemes 应被覆盖，got=%v", info.Schemes)
	}
}

func TestFill_TitleAndVersionDefaultToApp(t *testing.T) {
	// 同一个事实配两遍迟早会不一致
	xonetest.UseConfigYAML(t, "XApp:\n  Name: xone.demo.app\n  Version: v2.3.4\nXGinSwagger:\n  BasePath: /api\n")
	runStartHooks(t)
	c, err := fileConfig()
	if err != nil {
		t.Fatal(err)
	}

	info := &swag.Spec{}
	fill(info, c)
	if info.Title != "xone.demo.app" {
		t.Errorf("没配标题时应取 App.Name，got=%q", info.Title)
	}
	if info.Version != "v2.3.4" {
		t.Errorf("版本应取 App.Version，got=%q", info.Version)
	}
	if info.BasePath != "/api" {
		t.Errorf("BasePath 应生效，got=%q", info.BasePath)
	}
}

func TestRegister_OnlyReadsConfigBuildsNothing(t *testing.T) {
	var got *hook.Entry
	for _, e := range hook.Start() {
		if e.Pkg == "github.com/jupiter-eng/xone/xginswagger" {
			got = &e
		}
	}
	if got == nil {
		t.Fatal("没有登记启动钩子，XGinSwagger 那一块就没人读")
	}
	for _, e := range hook.Stop() {
		if e.Pkg == "github.com/jupiter-eng/xone/xginswagger" {
			t.Error("本包没有要关的资源，登记停止钩子会让它出现在退出序列里")
		}
	}
}

func TestConfig_KeepsAnnotationSchemesWhenUnset(t *testing.T) {
	// 回归用例。默认值原先是 ["https", "http"]，而默认值是预填进结构体的：
	// 配置里只写了 BasePath，Schemes 也等于配了，注解里的 @schemes 永远被盖掉
	xonetest.UseConfigYAML(t, "XGinSwagger:\n  BasePath: /api\n")
	c, err := fileConfig()
	if err != nil {
		t.Fatal(err)
	}

	info := &swag.Spec{Schemes: []string{"http"}}
	fill(info, c)
	if len(info.Schemes) != 1 || info.Schemes[0] != "http" {
		t.Errorf("没写 Schemes 就该保留注解里的，got=%v", info.Schemes)
	}
}
