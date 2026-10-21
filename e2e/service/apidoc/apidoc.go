// Package apidoc is the e2e service's Swagger documentation, written the way the docs.go swag init
// generates is: one template and one *swag.Spec. The values in the annotations (@title, @version,
// @host, @schemes and so on) are the ones pre-filled in SwaggerInfo here, and a test checks them to
// see that "a field left unconfigured keeps the annotation and only a configured one overrides it".
//
// Unlike what swag generates: there is no swag.Register in init here, and Register does it
// explicitly -- scripts/check.sh allows an init() in the integration packages alone
package apidoc

import (
	"sync"

	"github.com/swaggo/swag"
)

// The values from the annotations. The tests check against these constants (an e2e test imports no
// package of the service, so the values are written out once more in the test)
const (
	AnnotatedTitle       = "annotated title"
	AnnotatedVersion     = "annotated-1"
	AnnotatedHost        = "annotated.example"
	AnnotatedBasePath    = "/annotated"
	AnnotatedDescription = "annotated description"
)

const docTemplate = `{
    "schemes": {{ marshal .Schemes }},
    "swagger": "2.0",
    "info": {
        "description": "{{escape .Description}}",
        "title": "{{.Title}}",
        "version": "{{.Version}}"
    },
    "host": "{{.Host}}",
    "basePath": "{{.BasePath}}",
    "paths": {
        "/ping": {
            "get": {
                "summary": "readiness probe",
                "responses": {"200": {"description": "pong"}}
            }
        }
    }
}`

// SwaggerInfo is the metadata the annotations generate, handed to xginswagger.Register. @schemes in
// the annotations is http
var SwaggerInfo = &swag.Spec{
	Version:          AnnotatedVersion,
	Host:             AnnotatedHost,
	BasePath:         AnnotatedBasePath,
	Schemes:          []string{"http"},
	Title:            AnnotatedTitle,
	Description:      AnnotatedDescription,
	InfoInstanceName: "swagger",
	SwaggerTemplate:  docTemplate,
	LeftDelim:        "{{",
	RightDelim:       "}}",
}

var once sync.Once

// Register registers SwaggerInfo with swag. swag.Register panics on a second registration, so this
// registers once
func Register() {
	once.Do(func() { swag.Register(SwaggerInfo.InstanceName(), SwaggerInfo) })
}
