package e2e

import (
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/jupiter-eng/xone/e2e/harness"
)

// 注解里写的值，和 service/apidoc 里的一致（e2e 测试不 import 被测服务的包）
const (
	swaggerAnnotatedHost     = "annotated.example"
	swaggerAnnotatedBasePath = "/annotated"
)

// swaggerDoc GET doc.json 并解出要核对的几项
type swaggerDoc struct {
	Schemes  []string `json:"schemes"`
	Host     string   `json:"host"`
	BasePath string   `json:"basePath"`
	Info     struct {
		Title       string `json:"title"`
		Version     string `json:"version"`
		Description string `json:"description"`
	} `json:"info"`
}

func getSwaggerDoc(t *testing.T, p *harness.Process, prefix string) swaggerDoc {
	t.Helper()
	r := p.Get(t, prefix+"/swagger/doc.json")
	if r.Status != http.StatusOK {
		t.Fatalf("xginswagger/README.md XGinSwagger：doc.json 应挂在 %s/swagger/ 下，GET %s/swagger/doc.json 实际 %v", prefix, prefix, r)
	}
	var d swaggerDoc
	r.JSON(t, &d)
	return d
}

// xginswagger/README.md XGinSwagger：
//
//	URLPrefix  UI 挂载路径前缀，默认挂在 /swagger/*any；留空，或以 / 开头、不以 / 结尾，否则启动失败
//	Schemes    默认留空：沿用注解里的 @schemes，写了才覆盖
//	Title      默认取 App.Name；版本默认取自 App
//	没写的字段一律沿用注解里的值，写了才覆盖
func TestCoverage_SwaggerUIAndDocUnderURLPrefix_UnsetFieldsFromAnnotations(t *testing.T) {
	harness.Require(t)
	t.Parallel()

	t.Run("默认挂在 /swagger，注解里的 schemes、host、basePath 原样保留", func(t *testing.T) {
		t.Parallel()
		p := harness.Start(t, harness.Options{})
		ui := p.Get(t, "/swagger/index.html")
		if ui.Status != http.StatusOK || !strings.Contains(string(ui.Body), "swagger-ui") {
			t.Errorf("默认应在 /swagger/index.html 挂 Swagger UI，实际 status=%d", ui.Status)
		}
		d := getSwaggerDoc(t, p, "")
		if !slices.Equal(d.Schemes, []string{"http"}) {
			t.Errorf("Schemes 没配时应沿用注解里的 @schemes [http]，实际 %v", d.Schemes)
		}
		if d.Host != swaggerAnnotatedHost || d.BasePath != swaggerAnnotatedBasePath {
			t.Errorf("Host / BasePath 没配时应沿用注解（%s %s），实际 %q %q", swaggerAnnotatedHost, swaggerAnnotatedBasePath, d.Host, d.BasePath)
		}
		// 标题、版本默认取自 App（service/application.yml：xone.e2e.service / e2e）
		if d.Info.Title != "xone.e2e.service" || d.Info.Version != "e2e" {
			t.Errorf("Title / 版本默认取自 App（xone.e2e.service / e2e），实际 %q / %q", d.Info.Title, d.Info.Version)
		}
		t.Logf("数字：默认 doc.json schemes=%v host=%s title=%s version=%s", d.Schemes, d.Host, d.Info.Title, d.Info.Version)
	})

	t.Run("URLPrefix: /internal，写了的字段覆盖注解", func(t *testing.T) {
		t.Parallel()
		p := harness.Start(t, harness.Options{Overlay: `XGinSwagger:
  URLPrefix: /internal
  Schemes: [https]
  Host: api.example.com
  Title: E2E API
  Description: from config
`})
		if r := p.Get(t, "/internal/swagger/index.html"); r.Status != http.StatusOK {
			t.Errorf("URLPrefix: /internal 时 UI 应在 /internal/swagger/index.html，实际 %v", r.Status)
		}
		if r := p.Get(t, "/swagger/index.html"); r.Status != http.StatusNotFound {
			t.Errorf("URLPrefix: /internal 时默认的 /swagger/index.html 不该再有，实际 %d", r.Status)
		}
		d := getSwaggerDoc(t, p, "/internal")
		if !slices.Equal(d.Schemes, []string{"https"}) {
			t.Errorf("Schemes 写了 [https] 就该覆盖注解，实际 %v", d.Schemes)
		}
		if d.Host != "api.example.com" || d.Info.Title != "E2E API" || d.Info.Description != "from config" {
			t.Errorf("写了的 Host / Title / Description 应覆盖注解，实际 host=%q title=%q description=%q", d.Host, d.Info.Title, d.Info.Description)
		}
		if d.BasePath != swaggerAnnotatedBasePath {
			t.Errorf("没写的 BasePath 应沿用注解 %s，实际 %q", swaggerAnnotatedBasePath, d.BasePath)
		}
	})

	t.Run("URLPrefix 写错时启动失败", func(t *testing.T) {
		t.Parallel()
		for _, bad := range []string{"internal", "/internal/"} {
			stderr := covStartupError(t, harness.Options{Overlay: "XGinSwagger:\n  URLPrefix: " + bad + "\n"})
			faultMustContain(t, "URLPrefix: "+bad+" 的启动错误", stderr, "xginswagger", "URLPrefix must start with / and must not end with /", `"`+bad+`"`)
		}
	})
}
