// covapp 一次性任务形状的最小程序，给 e2e 测配置加载时机的几条规矩：
// e2e/service 在 main 里读完业务配置才调 MustRun、不带 WithConfigPath，
// 「提前读过之后再点名另一个文件」这一条用它测不到。
//
//	COVAPP_WITH_CONFIG_PATH=a.yml   交给 xone.WithConfigPath
//	COVAPP_READ_EARLY=1             xone.Run 之前先读一次 XApp 块
//
// 每读一次往标准输出写一行 JSON：{"msg":"covapp read","phase":"early|in_run","name":"<XApp.Name>"}。
// Start 读完就返回 nil（一次性任务），Run 出错时把错误写到 stderr、以 1 退出。
//
// 不用 xhook：scripts/check.sh 把登记钩子的目录当成集成包，集成包不许 import 根包
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"

	"github.com/jupiter-eng/xone"
	"github.com/jupiter-eng/xone/xapp"
	"github.com/jupiter-eng/xone/xconfig"
)

type appBlock struct {
	Name    string `yaml:"Name"`
	Version string `yaml:"Version"`
}

func emit(phase, name string) {
	b, _ := json.Marshal(map[string]string{"msg": "covapp read", "phase": phase, "name": name})
	fmt.Println(string(b))
}

type job struct{}

// Start 读的是 xapp 在启动钩子里读好的 XApp 块
func (job) Start(context.Context) error {
	emit("in_run", xapp.Name())
	return nil
}

func main() {
	var opts []xone.Option
	if p := os.Getenv("COVAPP_WITH_CONFIG_PATH"); p != "" {
		opts = append(opts, xone.WithConfigPath(p))
	}
	if os.Getenv("COVAPP_READ_EARLY") == "1" {
		var a appBlock
		if err := xconfig.Unmarshal("XApp", &a); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		emit("early", a.Name)
	}
	if err := xone.Run(job{}, opts...); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
