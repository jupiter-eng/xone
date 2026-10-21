package config

import (
	"os"
	"strings"

	"github.com/jupiter-eng/xone/xerror"
	"github.com/jupiter-eng/xone/xutil"
)

const (
	// ArgKey / EnvKey are the two ways of naming the configuration file explicitly
	ArgKey = "config"
	EnvKey = "XONE_CONFIG"

	// ProfileArgKey / ProfileEnvKey say which profiles are active, as Spring's
	// spring.profiles.active does. Several are separated by commas
	ProfileArgKey = "profile"
	ProfileEnvKey = "XONE_PROFILE"
)

// profiles decides which profiles are active: the command line > the environment > XApp.Profiles
// in the configuration file, and says where it came from along the way (which XONE_DEBUG prints).
//
// As in Spring, a later profile overrides an earlier one, so prod's values are what take effect in
// --profile=base,prod.
func profiles(fromFile []string) ([]string, string) {
	if v := fromArgs(ProfileArgKey); v != "" {
		return splitProfiles(v), "--" + ProfileArgKey
	}
	if v := os.Getenv(ProfileEnvKey); v != "" {
		return splitProfiles(v), ProfileEnvKey
	}
	return fromFile, AppKey + "." + ProfilesKey + " in the config file"
}

// splitProfiles cuts on commas and drops the whitespace and the empty entries
func splitProfiles(s string) []string {
	out := make([]string, 0, 2)
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// SearchPaths are the conventional locations looked at, in order, when none is named explicitly
var SearchPaths = []string{
	"conf/application.yml",
	"conf/application.yaml",
	"config/application.yml",
	"config/application.yaml",
	"application.yml",
	"application.yaml",
}

// locate finds the configuration file: the command line > the environment > the conventional
// paths, and says how it was found along the way (which XONE_DEBUG prints).
//
// With none found it returns an empty string, leaving the caller to decide between an error and
// starting on the defaults alone.
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

// fromArgs reads --key=value or --key value from the command line, accepting both spellings.
//
// The flag package is not used: flag.Parse takes the whole command line over, while a caller's
// program most likely has arguments of its own, and the framework must not decide how they are
// parsed.
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

// danglingArg is an error when one of the framework's flags is written last with no value.
//
// fromArgs finds no value in that spelling and treats it as not written: `./app --config` goes on
// to XONE_CONFIG and the conventional paths, and what comes up is another configuration, or one of
// nothing but defaults -- while whoever wrote it believes they named a file.
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
