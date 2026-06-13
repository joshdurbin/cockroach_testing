---
name: cobra-viper-zerolog
description: CLI framework conventions used in this project (russ pattern)
metadata: 
  node_type: memory
  type: project
  originSessionId: 3985f969-5856-416c-bbcb-d5b4c64ba8e9
---

## viper.BindPFlag after every flag definition

Every flag must be explicitly bound to viper after definition. Without this, `viper.GetX("key")` returns the zero value even when the flag is set:

```go
func init() {
    cmd.Flags().Int("tenants", 10, "number of tenants")
    viper.BindPFlag("workload.tenants", cmd.Flags().Lookup("tenants"))
    // ↑ required — cobra flags and viper are independent systems
}
```

## PersistentFlags bleed to ALL subcommands

`parent.PersistentFlags()` are inherited by every subcommand of parent. `cmd.Flags()` are local to that command only. Use PersistentFlags for cross-cutting concerns (cluster name, API addr, verbosity). Use Flags for command-specific options.

Mistake made: `--nodes` was on `chaosCmd.PersistentFlags()`, which made it appear on every `chaos inject *` subcommand even though only `chaos setup` needs it.

## SetEnvPrefix + AutomaticEnv for env overrides

```go
func initConfig() {
    viper.SetEnvPrefix("CDBCT")
    viper.AutomaticEnv()
    // Now CDBCT_VERBOSE=true overrides --verbose flag
}
```

`AutomaticEnv` maps env vars to viper keys: `CDBCT_CLUSTER_NAME` → `cluster.name`. Key separator in env is `_` → `.` in viper.

## OnInitialize for logging setup

```go
func init() {
    cobra.OnInitialize(initConfig)
}

func initConfig() {
    viper.SetEnvPrefix("CDBCT")
    viper.AutomaticEnv()
    log.Logger = log.Output(zerolog.ConsoleWriter{Out: os.Stderr, TimeFormat: time.RFC3339})
    if viper.GetBool("verbose") {
        zerolog.SetGlobalLevel(zerolog.DebugLevel)
    } else {
        zerolog.SetGlobalLevel(zerolog.InfoLevel)
    }
}
```

`OnInitialize` fires before any command's `RunE` but after flag parsing — correct place for viper/zerolog setup.

## Positional args vs flags: the naming principle

Use positional args for required nouns (the thing being acted on). Use flags for optional modifiers (how to act on it).

```
cdbct chaos inject partition 2          ← node is a noun, positional
cdbct chaos inject latency 2 --latency=200   ← node is positional, latency is modifier
cdbct cluster create --nodes=9          ← nodes is a modifier, positional
```

Implemented with `cobra.ExactArgs(1)` and `nodeArg(args)` helper that validates it's a positive integer.

## Zerolog structured fields

Use structured key-value fields, not string interpolation:
```go
log.Info().Str("cluster", cluster).Int("nodes", nodes).Msg("creating cluster")
// NOT: log.Info().Msgf("creating cluster %s with %d nodes", cluster, nodes)
```

This produces machine-parseable JSON in production and pretty console output in development.

## Command Use strings for help text

Include the positional arg name in `Use`:
```go
Use: "latency <node>",   // ← shows in help as: cdbct chaos inject latency <node>
Use: "clear [node]",     // ← brackets = optional
```

## viper.GetDuration for time.Duration flags

Cobra/viper natively handle `time.Duration` flags:
```go
cmd.Flags().Duration("interval", 100*time.Millisecond, "tick interval")
viper.BindPFlag("workload.interval", cmd.Flags().Lookup("interval"))
// Later:
interval := viper.GetDuration("workload.interval")  // returns time.Duration
```

Users can pass `--interval=100ms`, `--interval=1s`, `--interval=2m`.

## Shared flags across related commands via parent PersistentFlags

```go
// In workloadCmd.init():
workloadCmd.PersistentFlags().String("cluster-name", docker.DefaultCluster, "cluster name")
viper.BindPFlag("cluster.name", workloadCmd.PersistentFlags().Lookup("cluster-name"))
// Now workload start, workload stop, workload ls all inherit --cluster-name
```

## Root command Short vs Long

Short is shown in `--help` output and command listings. Keep it to one line. Long is shown in `help <command>` output — can be multiline. For this project, most commands only have Short since they're simple enough.
