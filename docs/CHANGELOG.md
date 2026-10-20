# 更新日志

这里记录每个版本里**使用者看得见的变化**：新功能、行为变化、不兼容变更、修复、性能。
仓库内部的重构和工具改动不写，除非它改变了使用者要做的事。

格式参考 [Keep a Changelog](https://keepachangelog.com/zh-CN/1.1.0/)，版本号遵循
[语义化版本](https://semver.org/lang/zh-CN/)。所有模块共用同一个版本号（见 `scripts/release.sh`）。

## [未发布]

## [v1.0.3] - 2026-10-20

### 新增

- xecho 新增配置项 `XEcho.Debug`（默认 `false`，同 echo 自己的默认）：打开后 echo 默认的错误处理会把错误原文放进响应，`c.JSON` / `c.JSONP` / `c.XML` 一律缩进输出，echo 自己的日志级别放宽到 DEBUG。线上别开，详见 [xecho/README.md「行为与实测」](../xecho/README.md#behavior-measured)。
- xecho 新增 `(*XEcho).WithValidator(echo.Validator)`：装上之后 handler 里可以 `c.Validate(&req)`。echo 不内置校验器，不装时 `c.Validate` 返回的是 `echo.ErrValidatorNotRegistered`（handler 直接返回它是 500）。选哪个校验库仍由业务自己定，xecho 不增加这方面的依赖。

## [v1.0.2] - 2026-10-13

### 其他

- 本版没有使用者可见的变化：代码与 v1.0.1 完全相同，只更新了仓库内部的发版工具。

## [v1.0.1] - 2026-10-13

### 修复

- v1.0.0 只推出了根模块的 tag，xgin、xgorm 这些集成模块没有，照 README 的安装命令装会失败；v1.0.1 是第一个各模块都装得上的版本。v1.0.0 已在根模块 `go.mod` 里撤回（`retract`）。

## [v1.0.0] - 2026-10-13

### 新增

- 首个版本。核心：`xone.Run` 的配置加载与生命周期，含 xapp、xconfig、xerror、xflow、xhook、xlog、xtls、xutil、xonetest。
- 集成模块：xgin、xecho、xginswagger、xgorm（含 clickhouse 驱动）、xredis、xcache、xhttp、xkafka、xcron、xmetric、xtrace。
