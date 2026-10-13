#!/usr/bin/env python3
# check.sh 第 8 步：找出运行期字符串里的中文，每处打一行「文件:行号」。
# 要查的文件从 GOFILES 环境变量拿（空白分隔）。
#
# 单独成文件而不是 check.sh 里的 heredoc：macOS 的 /bin/sh（bash 3.2）解析
# $(... <<'EOF' ...) 时会去配对 heredoc 里的反引号和引号，下面的正则一定有，
# 于是整份 check.sh 报语法错误，一步都不跑
import os
import re

zh = re.compile(r'[一-鿿]')
# 整个文件一次扫完，从左往右认记号：注释、"..."（带转义）、`...`（原始字符串，
# 可以跨行）、'...'（rune，认出来只为了 '"' 这样的不被当成字符串开头）。
# 一个记号认完才从它后面接着找，所以注释里的引号、字符串里的 // 都不会被误认。
# 从前是逐行 split('//') 再找 "..."：带 http:// 的字符串从 // 处被截断、
# 反引号的字符串根本不看，里面写中文照样放过
token = re.compile(r'//[^\n]*|/\*.*?\*/|"(?:[^"\\\n]|\\.)*"|`[^`]*`|\'(?:[^\'\\\n]|\\.)*\'', re.S)
for f in os.environ['GOFILES'].split():
    if f.endswith('_test.go'):
        continue
    # 构建工具不受这条约束：它的输出给的是改这份代码的人，
    # 不会落进使用者的日志里，而这条规矩的全部理由就是后者
    if f.startswith('internal/schemagen/'):
        continue
    src = open(f, encoding='utf-8').read()
    seen = set()
    for m in token.finditer(src):
        if m.group()[0] in '"`' and zh.search(m.group()):
            line = src.count('\n', 0, m.start()) + 1
            if line not in seen:
                seen.add(line)
                print(f"{f}:{line}")
