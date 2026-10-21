// covapp is a minimal program shaped like a one-off job, for the e2e cases about when the
// configuration is read: e2e/service reads its business configuration in main before calling
// MustRun and passes no WithConfigPath, so "another file named after the configuration has already
// been read" cannot be tested with it.
//
//	COVAPP_WITH_CONFIG_PATH=a.yml   goes to xone.WithConfigPath
//	COVAPP_READ_EARLY=1             reads the XApp block once before xone.Run
//
// Every read writes one JSON line to standard output:
// {"msg":"covapp read","phase":"early|in_run","name":"<XApp.Name>"}.
// Start returns nil as soon as it has read (a one-off job), and a failing Run writes the error to
// stderr and exits with 1.
//
// No xhook: scripts/check.sh takes a directory registering hooks for an integration package, and
// an integration package may not import the root package
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

// Start reads the XApp block xapp read in its start hook
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
