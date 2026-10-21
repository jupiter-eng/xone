# xconfig

Decodes an application's own block of the config file into a plain Go struct (a core module), by the same rules the framework's own config blocks follow.

- Defaults are pre-filled in the struct, and a field the file does not set is left unchanged
- A mistake fails startup: an unrecognized field, an unset `${VAR}`, a duration written as a bare number
- `${VAR}` is expanded as usual, and `${VAR:default}` is optional
- A struct implementing `Validate() error` has it called once after decoding
- It can be read at any time: the config file is loaded on the first read, so what is read is always the final value

## Quick start

```yaml
# conf/application.yml
Order:
  PayTimeout: 30m                        # durations carry a unit: 30s / 1500ms / 1h30m
  CallbackURL: "${ORDER_CALLBACK_URL}"   # a required environment variable; startup fails when it is unset
```

```go
type OrderConfig struct {
	PayTimeout  time.Duration `yaml:"PayTimeout"` // every field carries a yaml tag
	MaxItems    int           `yaml:"MaxItems"`
	CallbackURL string        `yaml:"CallbackURL"`
}

// Validate is called once after decoding: a misconfigured value fails at startup, before a single request is served
func (c OrderConfig) Validate() error {
	if c.MaxItems <= 0 {
		return errors.New("MaxItems must be > 0")
	}
	return nil
}

// in main, before xone.Run
cfg := OrderConfig{PayTimeout: 15 * time.Minute, MaxItems: 50} // the defaults, pre-filled
if err := xconfig.Unmarshal("Order", &cfg); err != nil {
	log.Fatal(err)
}
```

`MaxItems` is not set and keeps its default of 50; `PayTimeout` is changed to 30 minutes by the file.

## API

| Function | Description |
|---|---|
| `Unmarshal(key string, into any) error` | Decodes the `key` block into `into`. When the whole block is absent, `into` is left untouched and nil is returned |
| `Has(key string) bool` | Whether the file writes this block, for "do it only when configured". Asking counts as claiming it |
| `UnmarshalClients[C any](key string, defaults func() C) (map[string]C, error)` | Decodes a block written as a single instance or as several, returning `map[name]config` with the defaults pre-filled per instance. Mainly for writing integrations |
| `DecodeStrict(node *yaml.Node, v any) error` | Used inside a collection element's own `UnmarshalYAML` to keep the strict "a misspelled field fails" check |
| `DefaultClientName` | The instance name the single-instance form is normalized to, that is `"default"` |

To swap in another configuration in a test, use `xonetest.UseConfigYAML(t, yml)`; see [guide.md "Testing"](../docs/guide.md#testing).

## Notes

- **Every field carries a `yaml` tag.** Without one, yaml.v3 only recognizes an all-lowercase key, so `PayTimeout:` is
  taken for an unrecognized field and fails startup, with the error suggesting a tag; see
  [troubleshooting.md](../docs/troubleshooting.md#field-bogus-not-found-in-type-xginconfig).
- **Read it before `Start`**: in `main`, before `xone.Run`, or inside a `BeforeStart` hook (see [below](#reading-it-inside-a-hook)).
  Reading only inside `Start` does not count: once the start hooks have run, a top-level key nobody read fails startup
  (`config keys [Order] are not read by anyone`).
- **Durations carry a unit**: a bare `30` is an error outright (``cannot unmarshal !!int `30` into time.Duration``) and is
  never quietly taken for 30 nanoseconds.
- **To tell "not configured" from "configured", use `xconfig.Has("Order")`** rather than pointer fields.
- Reading in `main` has one advantage: the type does not have to know about `xconfig`, and a test can pass a value directly.

## Reading it inside a hook

Configuration used in several packages, or needing some initialization after it is read, goes into a start hook. Without a
stage that is `StageBusiness`: every client is ready and the servers are not yet taking traffic.

```go
var conf = Config{PayTimeout: 15 * time.Minute}

func init() {
	xhook.BeforeStart(func(ctx context.Context) error { // init only registers; xone.Run runs it by stage
		return xconfig.Unmarshal("Order", &conf)
	})
}

// C is used in Start and in request handling, by which time the start hooks have run
func C() Config { return conf }
```

Runnable examples: [`example/component/conf/`](../example/component/conf/) (read in `main`) and [`example/consumer/`](../example/consumer/).

## The config file rules

One line each; the details are all in [`docs/config.md`](../docs/config.md):

| Goal | How | Details |
|---|---|---|
| Take a value from the environment | `${VAR}` is required and fails startup when unset; `${VAR:default}` is optional | [Placeholders](../docs/config.md#placeholders) |
| One file per environment | `application-prod.yml` holds the differences, selected with `--profile=prod` or `XONE_PROFILE=prod` | [Profiles](../docs/config.md#profiles--one-file-per-environment), [a complete example](../docs/config.md#multi-environment-configuration-a-complete-example) |
| Split the configuration across files | `Import: [shared.yml, optional:local.yml]` | [Import](../docs/config.md#import--pulling-in-other-config-files) |
| Name the config file | `--config=<path>` or `XONE_CONFIG`; without one, the conventional paths such as `conf/application.yml` are searched | [File locations](../docs/config.md#file-locations-and-priority) |
| Know how several files layer | Maps merge recursively, lists are replaced whole, scalars override | [Merge rules](../docs/config.md#merge-rules) |

## Going further

- **Map values and slice elements need defaults too**: give the element type an `UnmarshalYAML` that pre-fills the defaults
  and then decodes with `xconfig.DecodeStrict` (`node.Decode` cannot be used; it drops the strict check):

  ```go
  func (r *Rule) UnmarshalYAML(n *yaml.Node) error {
  	*r = DefaultRule()
  	type raw Rule // a different type, otherwise this would call itself recursively
  	return xconfig.DecodeStrict(n, (*raw)(r))
  }
  ```

- **The single-instance and multi-instance forms** (as in XGorm): `UnmarshalClients` decides which to use by the presence
  of `Clients`, and mixing the two is an error. See
  [guide.md "Writing an integration of your own"](../docs/guide.md#writing-an-integration-of-your-own).
