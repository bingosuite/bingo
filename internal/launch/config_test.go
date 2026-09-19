package launch

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestResolve(t *testing.T) {
	root := t.TempDir()
	cwd := filepath.Join(root, "working directory")
	if err := os.Mkdir(cwd, 0o700); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, program, cwd, wantProgram, wantCwd string
	}{
		{"relative", "app with spaces", cwd, filepath.Join(cwd, "app with spaces"), cwd},
		{"absolute", filepath.Join(root, "app"), cwd, filepath.Join(root, "app"), cwd},
		{"empty cwd", filepath.Join(root, "app"), "", filepath.Join(root, "app"), ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			program, actualCwd, err := Resolve(tc.program, tc.cwd)
			if err != nil || program != tc.wantProgram || actualCwd != tc.wantCwd {
				t.Fatalf("Resolve = %q, %q, %v; want %q, %q", program, actualCwd, err, tc.wantProgram, tc.wantCwd)
			}
		})
	}
	absolute, err := filepath.Abs("relative-app")
	if err != nil {
		t.Fatal(err)
	}
	program, actualCwd, err := Resolve("relative-app", "")
	if err != nil || program != absolute || actualCwd != "" {
		t.Fatalf("legacy relative Resolve = %q, %q, %v", program, actualCwd, err)
	}
	for _, tc := range [][2]string{{"", cwd}, {"app\x00tail", cwd}, {"app", cwd + "\x00tail"}, {"app", filepath.Join(root, "missing")}} {
		if _, _, err := Resolve(tc[0], tc[1]); err == nil {
			t.Fatalf("Resolve(%q, %q) accepted invalid input", tc[0], tc[1])
		}
	}
	if err := os.WriteFile(filepath.Join(root, "file"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := Resolve("app", filepath.Join(root, "file")); err == nil {
		t.Fatal("accepted a file as cwd")
	}
}

func TestArgumentValidation(t *testing.T) {
	for _, tc := range []struct {
		args, env []string
		valid     bool
	}{
		{valid: true},
		{args: []string{"", "argument with spaces"}, env: []string{"EMPTY=", "VALUE=a=b"}, valid: true},
		{args: []string{"bad\x00arg"}},
		{env: []string{"NO_EQUALS"}},
		{env: []string{"=VALUE"}},
		{env: []string{"KEY=bad\x00value"}},
	} {
		if err := ValidateArguments(tc.args, tc.env); (err == nil) != tc.valid {
			t.Fatalf("ValidateArguments(%q, %q) = %v", tc.args, tc.env, err)
		}
	}
}

func TestEnvironmentAndChildDirectory(t *testing.T) {
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	parentCwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	cwd := filepath.Join(t.TempDir(), "child directory")
	if err := os.Mkdir(cwd, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cwd, "data.txt"), []byte("relative data"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, dir := range []string{cwd, ""} {
		cmd := exec.Command(executable, "-test.run=^TestDirectoryChild$")
		cmd.Dir = dir
		cmd.Env = Environment(dir, []string{
			"BINGO_DIRECTORY_CHILD=1", "BINGO_VALUE=first", "BINGO_VALUE=last",
		})
		if dir != "" {
			cmd.Env = Environment(dir, append(cmd.Env, "PWD=/stale/override"))
		}
		wire, err := cmd.Output()
		if err != nil {
			t.Fatal(err)
		}
		var actual map[string]string
		if err := json.Unmarshal(wire, &actual); err != nil {
			t.Fatal(err)
		}
		wantDir := dir
		if wantDir == "" {
			wantDir = parentCwd
		}
		want := map[string]string{"cwd": wantDir, "value": "last", "data": ""}
		if dir != "" {
			want["data"], want["pwd"] = "relative data", cwd
		} else {
			want["pwd"] = os.Getenv("PWD")
		}
		if !reflect.DeepEqual(actual, want) {
			t.Fatalf("child environment = %v, want %v", actual, want)
		}
	}
	if after, err := os.Getwd(); err != nil || after != parentCwd {
		t.Fatalf("parent cwd changed: %q, %v", after, err)
	}
	if env := Environment("", nil); !reflect.DeepEqual(env, os.Environ()) {
		t.Fatal("legacy launch changed inherited environment")
	}
}

func TestDirectoryChild(t *testing.T) {
	if os.Getenv("BINGO_DIRECTORY_CHILD") != "1" {
		return
	}
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile("data.txt")
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	result := map[string]string{"cwd": cwd, "pwd": os.Getenv("PWD"), "value": os.Getenv("BINGO_VALUE"), "data": strings.TrimSpace(string(data))}
	if err := json.NewEncoder(os.Stdout).Encode(result); err != nil {
		t.Fatal(err)
	}
	os.Exit(0)
}
