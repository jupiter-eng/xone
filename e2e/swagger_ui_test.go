package e2e

import (
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/jupiter-eng/xone/e2e/harness"
)

// The values from the annotations, matching service/apidoc (an e2e test imports no package of the service under test)
const (
	swaggerAnnotatedHost     = "annotated.example"
	swaggerAnnotatedBasePath = "/annotated"
)

// swaggerDoc GETs doc.json and decodes the few fields to check
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
		t.Fatalf("xginswagger/README.md, XGinSwagger: doc.json should sit under %s/swagger/, and GET %s/swagger/doc.json gave %v", prefix, prefix, r)
	}
	var d swaggerDoc
	r.JSON(t, &d)
	return d
}

// xginswagger/README.md, XGinSwagger:
//
//	URLPrefix  the prefix the UI is mounted under, /swagger/*any by default; empty, or starting with / and not ending with /, or the start fails
//	Schemes    empty by default: it keeps the @schemes from the annotations, and only a value set here overrides them
//	Title      taken from App.Name by default; the version comes from App by default
//	every field left unset keeps the value from the annotations, and only one that is set overrides it
func TestCoverage_SwaggerUIAndDocUnderURLPrefix_UnsetFieldsFromAnnotations(t *testing.T) {
	harness.Require(t)
	t.Parallel()

	t.Run("it is mounted under /swagger by default and the annotated schemes, host and basePath are kept", func(t *testing.T) {
		t.Parallel()
		p := harness.Start(t, harness.Options{})
		ui := p.Get(t, "/swagger/index.html")
		if ui.Status != http.StatusOK || !strings.Contains(string(ui.Body), "swagger-ui") {
			t.Errorf("the Swagger UI should be mounted at /swagger/index.html by default, got status=%d", ui.Status)
		}
		d := getSwaggerDoc(t, p, "")
		if !slices.Equal(d.Schemes, []string{"http"}) {
			t.Errorf("with Schemes unset it should keep the @schemes from the annotations, [http], got %v", d.Schemes)
		}
		if d.Host != swaggerAnnotatedHost || d.BasePath != swaggerAnnotatedBasePath {
			t.Errorf("with Host / BasePath unset they should keep the annotations (%s %s), got %q %q", swaggerAnnotatedHost, swaggerAnnotatedBasePath, d.Host, d.BasePath)
		}
		// The title and the version come from App by default (service/application.yml: xone.e2e.service / e2e)
		if d.Info.Title != "xone.e2e.service" || d.Info.Version != "e2e" {
			t.Errorf("Title / the version come from App by default (xone.e2e.service / e2e), got %q / %q", d.Info.Title, d.Info.Version)
		}
		t.Logf("numbers: the default doc.json schemes=%v host=%s title=%s version=%s", d.Schemes, d.Host, d.Info.Title, d.Info.Version)
	})

	t.Run("URLPrefix: /internal, and the fields that are set override the annotations", func(t *testing.T) {
		t.Parallel()
		p := harness.Start(t, harness.Options{Overlay: `XGinSwagger:
  URLPrefix: /internal
  Schemes: [https]
  Host: api.example.com
  Title: E2E API
  Description: from config
`})
		if r := p.Get(t, "/internal/swagger/index.html"); r.Status != http.StatusOK {
			t.Errorf("with URLPrefix: /internal the UI should be at /internal/swagger/index.html, got %v", r.Status)
		}
		if r := p.Get(t, "/swagger/index.html"); r.Status != http.StatusNotFound {
			t.Errorf("with URLPrefix: /internal the default /swagger/index.html should be gone, got %d", r.Status)
		}
		d := getSwaggerDoc(t, p, "/internal")
		if !slices.Equal(d.Schemes, []string{"https"}) {
			t.Errorf("Schemes set to [https] should override the annotations, got %v", d.Schemes)
		}
		if d.Host != "api.example.com" || d.Info.Title != "E2E API" || d.Info.Description != "from config" {
			t.Errorf("the Host / Title / Description that are set should override the annotations, got host=%q title=%q description=%q", d.Host, d.Info.Title, d.Info.Description)
		}
		if d.BasePath != swaggerAnnotatedBasePath {
			t.Errorf("BasePath was not set and should keep the annotation %s, got %q", swaggerAnnotatedBasePath, d.BasePath)
		}
	})

	t.Run("a malformed URLPrefix fails the start", func(t *testing.T) {
		t.Parallel()
		for _, bad := range []string{"internal", "/internal/"} {
			stderr := covStartupError(t, harness.Options{Overlay: "XGinSwagger:\n  URLPrefix: " + bad + "\n"})
			faultMustContain(t, "the start error for URLPrefix: "+bad, stderr, "xginswagger", "URLPrefix must start with / and must not end with /", `"`+bad+`"`)
		}
	})
}
