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

// runStartHooks runs each package's own start hooks: xapp reads its configuration itself.
func runStartHooks(t *testing.T) {
	t.Helper()
	for _, e := range hook.Start() {
		if err := e.Run(context.Background()); err != nil {
			t.Fatalf("%s failed: %v", e.Name, err)
		}
	}
}

func get(e *gin.Engine, path string) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	e.ServeHTTP(w, httptest.NewRequest("GET", path, nil))
	return w
}

func TestRegister_BeforeXoneRunInfoStillFromConfig(t *testing.T) {
	// xgin allows the engine to be assembled before xone.Run (a call to Engine() at the top of main).
	// The configuration can be read at that point, but the title, which defaults to App.Name, has to wait for
	// xapp's start hook -- filling it at the moment of Register would still open the document, only with the wrong title.
	testkit.UseConfigEnv(t, "XApp:\n  Name: swagger.lazy.app\nXGinSwagger:\n  Host: api.example.com\n")
	info := testSpec()

	e := gin.New()
	Register(e, info) // the configuration is not loaded yet, and xapp has not been read either
	runStartHooks(t)

	body := get(e, "/swagger/doc.json").Body.String()
	if !strings.Contains(body, "api.example.com") || !strings.Contains(body, "swagger.lazy.app") {
		t.Errorf("the metadata must come from the configuration and App, got=%s", body)
	}
}

// testSpec is the metadata registered on swag's default instance. swag does not allow registering twice,
// so the whole test process registers once and restores the fields on each use.
var (
	specOnce sync.Once
	spec     = &swag.Spec{InfoInstanceName: swag.Name, SwaggerTemplate: `{"host":"{{.Host}}","title":"{{.Title}}"}`}
)

func testSpec() *swag.Spec {
	specOnce.Do(func() { swag.Register(swag.Name, spec) })
	spec.Title, spec.Host = "the title from the annotations", "annotated"
	return spec
}

func TestRegister_BeforeXoneRunPrefixStillFromConfig(t *testing.T) {
	// The route is fixed at the moment of registration. Register used to read a package variable filled by the start
	// hook, so running earlier left it on the default prefix and then failed startup; now the configuration gives the final value whenever it is read.
	testkit.UseConfigEnv(t, "XGinSwagger:\n  URLPrefix: /internal\n")

	e := gin.New()
	Register(e, nil)

	if w := get(e, "/internal/swagger/index.html"); w.Code != http.StatusOK {
		t.Errorf("it must be mounted under the configured prefix, got=%d", w.Code)
	}
	if !strings.HasPrefix(URL(), "/internal/") {
		t.Errorf("URL must carry the prefix, got=%s", URL())
	}
}

func TestLoadConfig_StartupFailsOnMalformedPrefix(t *testing.T) {
	// gin would normalize them to /internal/swagger/*any itself, but what URL() concatenates would not match.
	for _, p := range []string{"internal", "/internal/"} {
		xonetest.UseConfigYAML(t, "XGinSwagger:\n  URLPrefix: "+p+"\n")
		if err := loadConfig(context.Background()); err == nil {
			t.Errorf("URLPrefix=%q must fail startup", p)
		}
	}
}

func TestRegister_TwoEnginesMountingOneSpecDoNotRace(t *testing.T) {
	// info is the spec swag registered globally while Register's Once belongs to the Register call, so a public
	// server and an admin one (what xgin's WithConfig is for) both mounting the UI fill the same object from two
	// goroutines the first time each is visited. What checks this is the race detector, which scripts/test.sh runs
	// with; there is no mutation for it, since a mutation round runs the tests without it.
	xonetest.UseConfigYAML(t, "XGinSwagger:\n  Host: api.example.com\n")
	info := testSpec()

	public, admin := gin.New(), gin.New()
	Register(public, info)
	Register(admin, info)

	var wg sync.WaitGroup
	for _, e := range []*gin.Engine{public, admin} {
		wg.Add(1)
		go func(e *gin.Engine) {
			defer wg.Done()
			get(e, "/swagger/index.html")
		}(e)
	}
	wg.Wait()

	if info.Host != "api.example.com" {
		t.Errorf("the metadata must still be filled from the configuration, got host=%q", info.Host)
	}
}

func TestRegister_MountsUI(t *testing.T) {
	xonetest.UseConfigYAML(t, "")
	e := gin.New()
	Register(e, nil)

	if w := get(e, "/swagger/index.html"); w.Code != http.StatusOK {
		t.Errorf("the Swagger UI must be reachable, got=%d", w.Code)
	}
}

func TestRegister_SupportsPrefix(t *testing.T) {
	xonetest.UseConfigYAML(t, "XGinSwagger:\n  URLPrefix: /internal\n")

	e := gin.New()
	Register(e, nil)

	if w := get(e, "/internal/swagger/index.html"); w.Code != http.StatusOK {
		t.Errorf("it must be mounted under the configured prefix, got=%d", w.Code)
	}
	if !strings.HasPrefix(URL(), "/internal/") {
		t.Errorf("URL must carry the prefix, got=%s", URL())
	}
}

func TestRegister_NilEngineDoesNotPanic(t *testing.T) {
	Register(nil, nil)
}

func TestFill_OverridesOnlyConfiguredFields(t *testing.T) {
	// An empty configuration block must not blank the document title written in the annotations.
	info := &swag.Spec{
		Title:       "the title from the annotations",
		Description: "the description from the annotations",
		BasePath:    "/v1",
		Host:        "annotated.example.com",
		Schemes:     []string{"http"},
	}
	fill(info, Config{})

	if info.Description != "the description from the annotations" || info.BasePath != "/v1" || info.Host != "annotated.example.com" {
		t.Errorf("a field that is not configured must keep the value from the annotations, got=%+v", info)
	}
	if len(info.Schemes) != 1 || info.Schemes[0] != "http" {
		t.Errorf("without Schemes configured the one from the annotations must be kept, got=%v", info.Schemes)
	}
}

func TestFill_ConfiguredValueOverrides(t *testing.T) {
	info := &swag.Spec{Title: "the title from the annotations", Host: "old"}
	fill(info, Config{
		Host: "api.example.com", BasePath: "/api/v2",
		Title: "the title from the configuration", Description: "the description from the configuration",
		Schemes: []string{"https"},
	})

	if info.Host != "api.example.com" || info.BasePath != "/api/v2" {
		t.Errorf("a configured field must override, got=%+v", info)
	}
	if info.Title != "the title from the configuration" || info.Description != "the description from the configuration" {
		t.Errorf("the title and the description must be overridden, got=%+v", info)
	}
	if len(info.Schemes) != 1 || info.Schemes[0] != "https" {
		t.Errorf("Schemes must be overridden, got=%v", info.Schemes)
	}
}

func TestFill_TitleAndVersionDefaultToApp(t *testing.T) {
	// Configuring the same fact twice ends in disagreement sooner or later.
	xonetest.UseConfigYAML(t, "XApp:\n  Name: xone.demo.app\n  Version: v2.3.4\nXGinSwagger:\n  BasePath: /api\n")
	runStartHooks(t)
	c, err := fileConfig()
	if err != nil {
		t.Fatal(err)
	}

	info := &swag.Spec{}
	fill(info, c)
	if info.Title != "xone.demo.app" {
		t.Errorf("without a configured title App.Name must be used, got=%q", info.Title)
	}
	if info.Version != "v2.3.4" {
		t.Errorf("the version must come from App.Version, got=%q", info.Version)
	}
	if info.BasePath != "/api" {
		t.Errorf("BasePath must take effect, got=%q", info.BasePath)
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
		t.Fatal("no start hook is registered, so nobody reads the XGinSwagger block")
	}
	for _, e := range hook.Stop() {
		if e.Pkg == "github.com/jupiter-eng/xone/xginswagger" {
			t.Error("this package has nothing to close, and registering a stop hook would put it in the shutdown sequence")
		}
	}
}

func TestConfig_KeepsAnnotationSchemesWhenUnset(t *testing.T) {
	// Regression case. The default used to be ["https", "http"], and defaults are pre-filled into the struct:
	// with only BasePath written in the configuration, Schemes counted as configured too and the @schemes from the annotations was always overridden.
	xonetest.UseConfigYAML(t, "XGinSwagger:\n  BasePath: /api\n")
	c, err := fileConfig()
	if err != nil {
		t.Fatal(err)
	}

	info := &swag.Spec{Schemes: []string{"http"}}
	fill(info, c)
	if len(info.Schemes) != 1 || info.Schemes[0] != "http" {
		t.Errorf("without Schemes written the one from the annotations must be kept, got=%v", info.Schemes)
	}
}
