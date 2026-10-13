# 变异表：一个 Go module 一个文件（core.py 是根模块），scripts/mutate.py 按文件名顺序读进来。
#
# 每条变异是一行 mutate(名字, 文件, 目录, 测试过滤, 改法...)：
#   文件      要改坏的源文件，相对仓库根
#   目录      在哪个目录下跑 go test ./...（通常是这个文件所在的 module，
#             也可以是依赖它的别的 module，比如在 ./example 里验 xtrace 的承诺）
#   测试过滤  交给 go test -run
#   改法      只写 swap() / cut()，它们自带「模式恰好匹配 N 处」的断言
#
# 这一行放在「目录」所属的 module 的那个文件里，mutate.py 会核对。
# 每条前面的注释写「这条承诺防的是哪次真出过的事」。
from typing import NamedTuple


class Stale(Exception):
    """变异模式没对上：这条承诺这一轮根本没被检查"""


def swap(old, new, count=1):
    """把 old 换成 new，并断言它恰好出现 count 次。

    断言是重点，不是顺手加的：重构挪走一段代码之后，模式会悄悄匹配不到，
    这条承诺从此再也没被检查过，而整轮照报绿——这个仓库里真发生过两次。
    匹配到的处数不对同样要拦：多匹配一处就是改坏了两个地方，
    测试红了也说明不了是哪一条承诺在起作用。
    """
    def edit(s):
        if (n := s.count(old)) != count:
            raise Stale(f"模式匹配到 {n} 处、期望 {count} 处，这条变异要跟着代码改：\n{old}")
        return s.replace(old, new)
    return edit


def cut(start, end_after):
    """删掉从 start 开始、到 end_after 那一段结束为止的整块代码"""
    def edit(s):
        if (n := s.count(start)) != 1:
            raise Stale(f"起点匹配到 {n} 处、期望 1 处：\n{start}")
        i = s.index(start)
        if (j := s.find(end_after, i)) < 0:
            raise Stale(f"找不到终点：\n{end_after}")
        return s[:i] + s[j + len(end_after):]
    return edit


class Row(NamedTuple):
    name: str
    file: str
    module: str
    filt: str
    edits: tuple
    section: str


ROWS = []
_section = ""


def section(title):
    global _section
    _section = title


def mutate(name, file, module, filt, *edits):
    ROWS.append(Row(name, file, module, filt, edits, _section))
