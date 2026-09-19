package dap

import (
	"encoding/json"
	"path/filepath"
	"testing"
)

func TestStartArgumentBoundaries(t *testing.T) {
	for _, tc := range []struct {
		request, args string
	}{
		{"launch", `{}`},
		{"launch", `{"program":"/app","mode":"test"}`},
		{"launch", `{"program":"/app","mode":""}`},
		{"launch", `{"program":"/app","mode":null}`},
		{"launch", `{"program":"/app","cwd":null}`},
		{"launch", `{"program":"/app","cwd":42}`},
		{"launch", `{"program":"/app","cwd":"bad\u0000dir"}`},
		{"launch", `{"program":"/app","session":"session"}`},
		{"launch", `{"program":"/app","pid":0}`},
		{"launch", `{"program":"/app","noDebug":true}`},
		{"launch", `{"program":"/app","args":[null]}`},
		{"launch", `{"program":"/app","args":["bad\u0000arg"]}`},
		{"launch", `{"program":"/app","env":["NOT_AN_ASSIGNMENT"]}`},
		{"launch", `{"program":"/app","env":[null]}`},
		{"attach", `{}`},
		{"attach", `{"pid":0}`},
		{"attach", `{"pid":-1}`},
		{"attach", `{"pid":2147483648}`},
		{"attach", `{"pid":1.5}`},
		{"attach", `{"pid":null}`},
		{"attach", `{"session":""}`},
		{"attach", `{"session":"   "}`},
		{"attach", `{"session":"session","pid":0}`},
		{"attach", `{"session":"session","pid":42}`},
		{"attach", `{"session":"session","binaryPath":"/app"}`},
		{"attach", `{"pid":42,"mode":"exec"}`},
		{"attach", `{"pid":42,"cwd":"/tmp"}`},
		{"attach", `{"pid":42,"program":"/app","binaryPath":"/another"}`},
	} {
		t.Run(tc.request+"/"+tc.args, func(t *testing.T) {
			cfg, err := decodeStartConfig(json.RawMessage(tc.args))
			if err == nil {
				if tc.request == "launch" {
					_, err = prepareLaunch(cfg)
				} else {
					err = validateAttach(cfg)
				}
			}
			if err == nil {
				t.Fatal("invalid startup arguments accepted")
			}
		})
	}
	for _, args := range []string{`{"pid":42}`, `{"pid":42,"program":"/legacy/binary"}`, `{"session":"session"}`} {
		cfg, err := decodeStartConfig(json.RawMessage(args))
		if err != nil || validateAttach(cfg) != nil {
			t.Fatalf("valid attach rejected: %s, %v", args, err)
		}
	}
}

func TestPrepareLaunchPathsAndDefaults(t *testing.T) {
	dir := sourceFixture(t, "package main\nfunc main() {}\n")
	parent := filepath.Dir(dir)
	for _, tc := range []struct {
		name, mode, program, cwd, wantCwd string
	}{
		{"source default", "debug", dir, "", dir},
		{"source relative", "debug", filepath.Base(dir), parent, parent},
		{"source explicit", "debug", dir, parent, parent},
		{"legacy binary", "", filepath.Join(dir, "app"), "", ""},
		{"explicit binary", "exec", "app", dir, dir},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg, err := prepareLaunch(launchConfig{Mode: tc.mode, Program: tc.program, Cwd: tc.cwd})
			if err != nil || cfg.Cwd != tc.wantCwd || !filepath.IsAbs(cfg.Program) {
				t.Fatalf("prepared launch = %+v, %v", cfg, err)
			}
			wantProgram := dir
			if tc.mode != "debug" {
				wantProgram = filepath.Join(dir, "app")
			}
			if cfg.Program != wantProgram {
				t.Fatalf("program = %q, want %q", cfg.Program, wantProgram)
			}
		})
	}
	for _, program := range []string{filepath.Join(dir, "main.go"), "example.invalid/remote/package", filepath.Join(dir, "missing")} {
		if _, err := prepareLaunch(launchConfig{Mode: "debug", Program: program, Cwd: dir}); err == nil {
			t.Fatalf("source accepted non-directory program %q", program)
		}
	}
}
