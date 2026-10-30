package cli

import (
	"reflect"
	"testing"

	"github.com/xiws/ant/internal/model"
)

func TestParseArgsPromptAndFlags(t *testing.T) {
	opts, err := parseArgs([]string{"-p", "alpha", "-m", "plan", "给", "Test", "加", "边界检查"})
	if err != nil {
		t.Fatal(err)
	}
	if opts.command != "" || opts.model != "alpha" || opts.mode != model.ModePlan {
		t.Fatalf("opts = %+v", opts)
	}
	if opts.prompt != "给 Test 加 边界检查" {
		t.Fatalf("prompt = %q", opts.prompt)
	}
}

func TestParseArgsSubcommands(t *testing.T) {
	opts, err := parseArgs([]string{"new", "--new", "-m", "council", "--node-model", "code=alpha", "--node-model=verifier=beta", "do", "it"})
	if err != nil {
		t.Fatal(err)
	}
	if opts.command != "new" || opts.mode != model.ModeCouncil || !opts.newSession {
		t.Fatalf("opts = %+v", opts)
	}
	want := map[string]string{"code": "alpha", "verifier": "beta"}
	if !reflect.DeepEqual(opts.nodeModels, want) {
		t.Fatalf("nodeModels = %v", opts.nodeModels)
	}
	if opts.prompt != "do it" {
		t.Fatalf("prompt = %q", opts.prompt)
	}

	opts, err = parseArgs([]string{"sessions", "--limit", "5", "--mode", "code", "--status", "idle", "--json"})
	if err != nil {
		t.Fatal(err)
	}
	if opts.command != "sessions" || opts.limit != 5 || opts.filterMode != model.ModeCode || opts.filterStatus != "idle" || !opts.jsonOut {
		t.Fatalf("opts = %+v", opts)
	}

	opts, err = parseArgs([]string{"show", "20261030-094704", "--events"})
	if err != nil {
		t.Fatal(err)
	}
	if opts.command != "show" || opts.rest[0] != "20261030-094704" || !opts.showEvents {
		t.Fatalf("opts = %+v", opts)
	}
}

func TestParseArgsDashDash(t *testing.T) {
	opts, err := parseArgs([]string{"--", "sessions", "--new"})
	if err != nil {
		t.Fatal(err)
	}
	if opts.command != "" || opts.prompt != "sessions --new" {
		t.Fatalf("opts = %+v", opts)
	}
}

func TestParseArgsErrors(t *testing.T) {
	for _, args := range [][]string{
		{"--unknown"},
		{"-m", "bogus"},
		{"--log-level", "verbose"},
		{"--node-model", "bad"},
		{"--limit", "-1"},
	} {
		if _, err := parseArgs(args); err == nil {
			t.Fatalf("args %v should fail", args)
		}
	}
}

func TestParseArgsHelpVersion(t *testing.T) {
	opts, err := parseArgs([]string{"--help"})
	if err != nil || opts.command != "help" {
		t.Fatalf("help: %+v %v", opts, err)
	}
	opts, err = parseArgs([]string{"version"})
	if err != nil || opts.command != "version" {
		t.Fatalf("version: %+v %v", opts, err)
	}
	opts, err = parseArgs([]string{"new", "-h"})
	if err != nil || !opts.help {
		t.Fatalf("new -h: %+v %v", opts, err)
	}
}
