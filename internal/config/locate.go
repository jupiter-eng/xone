package config

import (
	"os"
	"strings"

	"github.com/jupiter-eng/xone/xerror"
	"github.com/jupiter-eng/xone/xutil"
)

const (
	// ArgKey / EnvKey 显式指定配置文件位置的两种方式
	ArgKey = "config"
	EnvKey = "XONE_CONFIG"

	// ProfileArgKey / ProfileEnvKey 指定激活哪些 profile，
	// 对应 Spring 的 spring.profiles.active。多个用逗号分隔
	ProfileArgKey = "profile"
	ProfileEnvKey = "XONE_PROFILE"
)

// profiles 决定激活哪些 profile：启动参数 > 环境变量 > 配置文件里的 XApp.Profiles，
// 另外说明是从哪来的（XONE_DEBUG 打出来）。
//
// 与 Spring 一致：靠后的 profile 压过靠前的，
// 所以 --profile=base,prod 里 prod 的值最终生效。
func profiles(fromFile []string) ([]string, string) {
	if v := fromArgs(ProfileArgKey); v != "" {
		return splitProfiles(v), "--" + ProfileArgKey
	}
	if v := os.Getenv(ProfileEnvKey); v != "" {
		return splitProfiles(v), ProfileEnvKey
	}
	return fromFile, AppKey + "." + ProfilesKey + " in the config file"
}

// splitProfiles 按逗号拆开，去掉空白和空项
func splitProfiles(s string) []string {
	out := make([]string, 0, 2)
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// SearchPaths 未显式指定时，按顺序查找的约定位置
var SearchPaths = []string{
	"conf/application.yml",
	"conf/application.yaml",
	"config/application.yml",
	"config/application.yaml",
	"application.yml",
	"application.yaml",
}

// locate 定位配置文件：启动参数 > 环境变量 > 约定路径，另外说明是怎么找到的（XONE_DEBUG 打出来）。
//
// 都找不到时返回空串，由调用方决定是报错还是全用默认值起。
func locate() (path, from string) {
	if v := fromArgs(ArgKey); v != "" {
		return v, "--" + ArgKey
	}
	if v := os.Getenv(EnvKey); v != "" {
		return v, EnvKey
	}
	for _, p := range SearchPaths {
		if xutil.FileExist(p) {
			return p, "default search path"
		}
	}
	return "", ""
}

// fromArgs 从命令行读 --key=value 或 --key value，两种写法都支持。
//
// 不用 flag 包：flag.Parse 会接管整个命令行，而使用者的程序多半有自己的参数，
// 框架不该替他们决定怎么解析。
func fromArgs(key string) string {
	args := os.Args[1:]
	for i, a := range args {
		if !strings.HasPrefix(a, "-") {
			continue
		}
		name := strings.TrimLeft(a, "-")
		if after, ok := strings.CutPrefix(name, key+"="); ok {
			return after
		}
		if name == key && i+1 < len(args) {
			return args[i+1]
		}
	}
	return ""
}

// danglingArg 框架认的启动参数写在了最后、没带值时报错。
//
// fromArgs 对这种写法取不到值，于是当它没写：`./app --config` 接着按 XONE_CONFIG、
// 约定路径找，起来的是另一份配置，或者一份全是默认值的——写的人以为自己点名了。
func danglingArg() error {
	args := os.Args[1:]
	if len(args) == 0 {
		return nil
	}
	last := args[len(args)-1]
	for _, key := range []string{ArgKey, ProfileArgKey} {
		if strings.HasPrefix(last, "-") && strings.TrimLeft(last, "-") == key {
			return xerror.Newf("xconfig", "config", "--%s needs a value: write --%s=<value> or --%s <value>", key, key, key)
		}
	}
	return nil
}
