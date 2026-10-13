package main

import (
	"encoding/json"
	"strings"

	"github.com/google/jsonschema-go/jsonschema"
)

// compile 把生成的 schema 交给 jsonschema-go 解析，校验配置用的是一个完整的 draft-07 实现，
// 而不是只认这个生成器产出的那几个关键字的手写子集
func compile(r *root) (*jsonschema.Resolved, error) {
	b, err := json.Marshal(r)
	if err != nil {
		return nil, err
	}
	var s jsonschema.Schema
	if err := json.Unmarshal(b, &s); err != nil {
		return nil, err
	}
	return s.Resolve(nil)
}

// problems 一份解析好的 YAML 过不过得了 schema，过不了时返回问题列表
func problems(rs *jsonschema.Resolved, doc any) []string {
	if err := rs.Validate(doc); err != nil {
		return strings.Split(err.Error(), "\n")
	}
	return nil
}
