#!/usr/bin/env python3
# 变异测试：把每一条承诺对应的代码改坏，看有没有测试会失败。
#
#   scripts/mutate.py                 改坏、编译、跑测试，默认开 CPU 数那么多路并行
#   scripts/mutate.py -j 2            只开两路
#   scripts/mutate.py --only xgorm    只跑某个表 / module / 路径前缀下的（可以写多个）
#   scripts/mutate.py -k 超时         只跑名字里带这段的（可以写多个）
#   scripts/mutate.py --dry-run       只在内存里套一遍模式，不跑 go，一秒以内（过滤照样生效）
#
# 活下来的变异 = 一条没有牙齿的承诺：代码写着、文档写着，但改坏了没人知道。
# 这个仓库前后被外部 review 挑出过二十多个问题，事后归类，绝大多数都是
# 「承诺有、测试没有」。与其每次等人来挑，不如让它自己说出来。
#
# 全量要跑一阵子，不在每次提交的 CI 里：nightly 跑一次，改完一批安全或生命周期
# 相关的代码之后手动跑一次。--dry-run 不一样：它只查「每条变异的模式是不是
# 恰好匹配 N 处」，check.sh 每次都跑，重构挪走了代码当场就知道。
#
# 只有「我们自己有代码在守」的承诺才适合放进来。像「ctx 能给整次逻辑请求
# 封顶」那种由依赖库保证的性质，这里没有哪一行可以改坏，硬写一条变异
# 只会让「活下来 = 缺测试」这个信号失真——那种承诺靠测试守着就行，
# 它防的是升级依赖时的回归，不是防我们自己改错。
#
# 变异表在 scripts/mutations/ 下，一个 module 一个文件，写法见那里的 __init__.py。
#
# 改坏的文件不写进工作区：改后的副本放在临时目录，经 go test -overlay 换进编译。
# 量过的（go 1.25）：
#   - 编译、go test 自带的那组 vet、经 replace 依赖进来的别的 module 都认 overlay——
#     在 ./xgin 里跑，xlog/config.go 的替身照样编进去；
#   - 构建缓存按替身的内容记：带 overlay 跑完再不带跑，拿到的是原来的代码，
#     几路并行共用一份缓存也互不干扰；
#   - runtime.Caller 报的仍是原路径；只有编译器和 vet 的报错里是替身的路径，打印时换回原路径；
#   - 只换得进编译：测试在运行时从磁盘读的源码看不到替身。目前没有测试读被变异的源文件
#     （schemagen 读的是别的 module 的 Config，它自己被变异的是 schema.go / docs.go）。
# 所以工作区不必干净、跑的时候照样可以改代码，被打断了也没有要写回的东西。
import argparse
import concurrent.futures
import importlib
import json
import os
import pathlib
import subprocess
import sys
import tempfile
import time

ROOT = pathlib.Path(__file__).resolve().parent.parent
sys.dont_write_bytecode = True  # 别在 scripts/ 下留 __pycache__
sys.path.insert(0, str(ROOT / "scripts"))
import mutations  # noqa: E402
from mutations import Stale  # noqa: E402


def load():
    """按文件名顺序读 scripts/mutations/*.py，返回 [(表名, Row)]"""
    rows = []
    for path in sorted((ROOT / "scripts" / "mutations").glob("[!_]*.py")):
        mutations.section("")
        before = len(mutations.ROWS)
        importlib.import_module(f"mutations.{path.stem}")
        rows += [(path.stem, r) for r in mutations.ROWS[before:]]
    return rows


def table_of(module):
    """跑测试的目录属于哪个 Go module，这一行就该放在哪个表：根模块是 core，其余取 module 目录名"""
    d = (ROOT / module).resolve()
    while not (d / "go.mod").exists():
        d = d.parent
    return "core" if d == ROOT else d.name


def picked(table, r, only, keys):
    def under(p):
        p = p.removeprefix("./").rstrip("/")
        return any(s == p or s.startswith(p + "/") for s in (table, r.module.removeprefix("./"), r.file))
    return (not only or any(map(under, only))) and (not keys or any(k in r.name for k in keys))


def apply(r):
    """在内存里套一遍改法，返回 (改后的内容, None)；模式失效时返回 (None, 说明)"""
    orig = (ROOT / r.file).read_text(encoding="utf-8")
    try:
        s = orig
        for e in r.edits:
            s = e(s)
    except Stale as err:
        return None, f"（变异没应用上，改坏的位置可能已经不在了）\n      {err}"
    # 改不动和「改坏了没人发现」一样严重：这条承诺这一轮根本没被检查，
    # 而脚本从前照样报绿。重构挪走了一段代码，对应的变异就这样悄悄失效了
    if s == orig:
        return None, "（变异没改动任何东西，模式失效了）"
    return s, None


def run(r, mutated, tmp):
    """跑一条变异，返回 (killed / survived / broken, 附在结果下面的几行)"""
    fake = tmp / r.file
    fake.parent.mkdir(parents=True)
    fake.write_text(mutated, encoding="utf-8")
    overlay = tmp / "overlay.json"
    overlay.write_text(json.dumps({"Replace": {str(ROOT / r.file): str(fake)}}))

    def go_test(*args):
        return subprocess.run(["go", "test", "-count=1", f"-overlay={overlay}", *args, "./..."],
                              cwd=ROOT / r.module, env=dict(os.environ, GOWORK="off"),
                              capture_output=True, text=True, errors="replace")  # 测试输出里可能有非 UTF-8 的字节

    # 先只编译不跑（-exec true：测试二进制照常编出来、连同 go test 自带的那组
    # vet 检查，但交给 true 去「执行」）。编不过的变异从前被算作「被杀掉」：
    # 测试红了，可红的原因是 declared and not used，不是哪条测试察觉了行为变化——
    # 这条承诺这一轮同样根本没被检查，和 stale 一样严重，单独报出来
    build = go_test("-exec", "true")
    if build.returncode != 0:
        lines = (build.stdout + build.stderr).replace(str(fake), r.file).splitlines()
        return "broken", [l for l in lines if not l.startswith(("ok ", "FAIL", "? "))][:3]
    # -timeout 是必须的：有些变异会让测试挂住而不是失败（比如把 ctx 换成
    # Background，等信号的那一步就永远等不到）。没有上限的话一个这样的变异
    # 就能把整轮跑死在那里。挂住同样说明测试察觉到了，算作被杀掉
    if go_test("-timeout", "90s", "-run", r.filt).returncode == 0:
        return "survived", []
    return "killed", []


MARK = {"killed": "✓", "survived": "✗", "stale": "?", "broken": "!"}
NOTE = {"survived": " —— 改坏了但测试全过", "broken": "（变异后编译不过，这一轮根本没检查到）"}


def main():
    sys.stdout.reconfigure(line_buffering=True)  # 接到 tee / 文件时也要逐条看到进度
    ap = argparse.ArgumentParser(description="变异测试：把每条承诺改坏，看有没有测试会失败")
    ap.add_argument("--dry-run", action="store_true", help="只核对模式，不跑 go")
    ap.add_argument("-j", type=int, default=os.cpu_count() or 1, metavar="N", help="并行跑几条，默认 CPU 数")
    ap.add_argument("--only", action="append", default=[], metavar="PREFIX",
                    help="只跑这个表（core、xgorm、clickhouse……）、module 或路径前缀下的")
    ap.add_argument("-k", action="append", default=[], metavar="TEXT", help="只跑名字里带这段的")
    args = ap.parse_args()
    t0 = time.monotonic()

    rows = load()
    misplaced = [f"{t}.py: {r.name}（在 {r.module} 里跑，属于 {table_of(r.module)}.py）"
                 for t, r in rows if table_of(r.module) != t]
    if misplaced:
        sys.exit("✗ 这些变异放错了表：\n  " + "\n  ".join(misplaced))
    rows = [(t, r) for t, r in rows if picked(t, r, args.only, args.k)]
    if not rows:
        sys.exit("✗ 过滤之后一条变异都不剩")

    got = dict.fromkeys(MARK, 0)
    # 模式先在内存里全部核对一遍：失效的不必排队等编译，dry-run 到这里就结束
    plan = []
    for t, r in rows:
        mutated, why = apply(r)
        if why:
            print(f"  ? {t} · {r.name}{why}")
            got["stale"] += 1
        else:
            plan.append((t, r, mutated))

    if args.dry_run:
        got["killed"] += len(plan)
    else:
        # 结果按表里的顺序打印，不按完成的先后：两轮的输出可以直接 diff
        with tempfile.TemporaryDirectory(prefix="xone-mutate-") as tmp, \
                concurrent.futures.ThreadPoolExecutor(max(1, args.j)) as pool:
            futures = [pool.submit(run, r, mutated, pathlib.Path(tmp, str(i)))
                       for i, (_, r, mutated) in enumerate(plan)]
            header = None
            try:
                for (t, r, _), f in zip(plan, futures):
                    if header != (t, r.section):
                        header = (t, r.section)
                        print(f"== {t} · {r.section} ==")
                    status, detail = f.result()
                    got[status] += 1
                    print(f"  {MARK[status]} {r.name}{NOTE.get(status, '')}")
                    for line in detail:
                        print(f"      {line}")
            except KeyboardInterrupt:
                # go test 和脚本在同一个进程组，Ctrl-C 已经送到它们了；还没开始的别再开
                pool.shutdown(wait=False, cancel_futures=True)
                sys.exit(130)

    total = sum(got.values())
    took = f"{time.monotonic() - t0:.1f}s"
    if got["killed"] == total:
        if args.dry_run:
            print(f"✓ {total} 条变异的模式都恰好对得上（{took}）")
        else:
            print(f"\n✓ {total} 条承诺全部有测试盯着（{took}，-j {args.j}）")
        return
    print()
    if got["survived"]:
        print(f"✗ {got['survived']} 条改坏了也没人发现")
    if got["stale"]:
        print(f"✗ {got['stale']} 条的变异模式已经失效，这一轮根本没检查到（多半是重构挪走了那段代码）")
    if got["broken"]:
        print(f"✗ {got['broken']} 条变异后编译不过，测试红是因为编不过而不是察觉了行为变化，改变异让它编得过")
    print(f"  —— 共 {total} 条（{took}）")
    sys.exit(1)


if __name__ == "__main__":
    main()
