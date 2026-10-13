# 更新日志

这里记录每个版本里**使用者看得见的变化**：新功能、行为变化、不兼容变更、修复、性能。
仓库内部的重构和工具改动不写，除非它改变了使用者要做的事。

格式参考 [Keep a Changelog](https://keepachangelog.com/zh-CN/1.1.0/)，版本号遵循
[语义化版本](https://semver.org/lang/zh-CN/)。所有模块共用同一个版本号（见 `scripts/release.sh`）。

## [未发布]

## [v1.0.1] - 2026-10-13

### 修复

- v1.0.0 只推出了根模块的 tag，xgin、xgorm 这些集成模块没有，照 README 的安装命令装会失败；v1.0.1 是第一个各模块都装得上的版本。v1.0.0 已在根模块 `go.mod` 里撤回（`retract`）。

## [v1.0.0] - 2026-10-13

### 新增

- 首个版本。核心：`xone.Run` 的配置加载与生命周期，含 xapp、xconfig、xerror、xflow、xhook、xlog、xtls、xutil、xonetest。
- 集成模块：xgin、xecho、xginswagger、xgorm（含 clickhouse 驱动）、xredis、xcache、xhttp、xkafka、xcron、xmetric、xtrace。
