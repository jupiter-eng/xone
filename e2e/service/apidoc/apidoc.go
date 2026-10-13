// Package apidoc 是 e2e 服务的 Swagger 文档，写法照 swag init 生成的 docs.go：
// 一份模板加一个 *swag.Spec。注解里写的值（@title、@version、@host、@schemes……）
// 就是 SwaggerInfo 里预填的这些，测试拿它们核对「没配的字段沿用注解、配了的才覆盖」。
//
// 和 swag 生成的不同：这里不在 init 里 swag.Register，由 Register 显式登记——
// scripts/check.sh 只许集成包有 init()
package apidoc

import (
	"sync"

	"github.com/swaggo/swag"
)

// 注解里的值。测试按这几个常量核对（e2e 测试不 import 服务的包，值在测试里另抄一份）
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

// SwaggerInfo 注解生成的元信息，交给 xginswagger.Register。注解里的 @schemes 是 http
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

// Register 把 SwaggerInfo 登记进 swag。swag.Register 重复登记会 panic，所以只登记一次
func Register() {
	once.Do(func() { swag.Register(SwaggerInfo.InstanceName(), SwaggerInfo) })
}
