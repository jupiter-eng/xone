# The example mutations: the promises of the example module. The format is in
# scripts/mutations/__init__.py
from . import cut, mutate, section, swap

section("Startup and shutdown")
mutate("a producer still publishing on exit does not crash it", "example/consumer/queue.go", "./example", "TestQueue",
       swap('''\tselect {
\tcase <-q.done: // already closed, this one is dropped
\tcase q.ch <- m:
\t}''', '\tq.ch <- m'),
       swap('\t\tclose(q.done)', '\t\tclose(q.ch)'))

section("Middleware")
# The two modules meet through the method name TrustedPeer(), and each one's unit tests see only
# its own half. Renaming it on one side leaves the other side's tests green -- only the end-to-end
# case in example sees it
mutate("xgin's and xtrace's trust marker agree", "xtrace/propagator.go", "./example", "TestXGinAndXTrace_ForwardHeadersOnlyFromTrustedPeers",
       swap('\tt, ok := c.(interface{ TrustedPeer() bool })\n\treturn ok && t.TrustedPeer()', '\tt, ok := c.(interface{ TrustedUpstream() bool })\n\treturn ok && t.TrustedUpstream()'))

section("The examples")
# select picks at random among the cases ready at the same time: on select alone, a worker keeps
# taking messages after the shutdown signal
mutate("no message is taken once the ctx is cancelled", "example/consumer/queue.go", "./example", "TestQueue",
       swap('\tif ctx.Err() != nil {\n\t\treturn Message{}, false\n\t}\n\tselect {', '\tselect {'))
# Neither of these two produces an error; the service merely "does nothing, normally"
mutate("a Workers that is not greater than 0 fails startup", "example/consumer/conf/conf.go", "./example", "TestLoad_",
       swap('if c.Workers <= 0 {', 'if false {'))
mutate("a MessageTimeout that is not greater than 0 fails startup", "example/consumer/conf/conf.go", "./example", "TestLoad_",
       swap('if c.MessageTimeout <= 0 {', 'if false {'))
# A stop hook left unregistered: the last batch of changes is never flushed on exit, while a test
# calling closeXKV directly stays green
mutate("xkv registers its stop hook", "example/component/xkv/xkv.go", "./example", "TestRegister_",
       swap('\txhook.BeforeStop(closeXKV) // the stage is inherited from the start hook above\n', ''))
# With a FlushInterval of 0, time.NewTicker panics inside a background goroutine and the process
# simply dies
mutate("a flush interval that is not greater than 0 fails startup", "example/component/xkv/xkv.go", "./example", "TestInitXKV|TestNew_",
       swap('if c.FlushInterval <= 0 {', 'if false {'))
mutate("a New called directly validates the configuration too", "example/component/xkv/xkv.go", "./example", "TestNew_",
       swap('\tif err := c.Validate(); err != nil {\n\t\treturn nil, nil, err\n\t}\n', ''))
# Flushing clears the dirty flag before writing: without handing it back when the write fails, this
# batch of changes is not even flushed by Close
mutate("a failed flush loses no data", "example/component/xkv/xkv.go", "./example", "TestStore",
       swap('\t\ts.mu.Lock()\n\t\ts.dirty = true\n\t\ts.mu.Unlock()\n', ''))
