package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

type workflow struct {
	On map[string]struct {
		Types  []string `yaml:"types"`
		Inputs map[string]struct {
			Default any `yaml:"default"`
		} `yaml:"inputs"`
	} `yaml:"on"`
	Permissions map[string]string      `yaml:"permissions"`
	Jobs        map[string]workflowJob `yaml:"jobs"`
}

type workflowJob struct {
	If          string            `yaml:"if"`
	Needs       any               `yaml:"needs"`
	Permissions map[string]string `yaml:"permissions"`
	Steps       []workflowStep    `yaml:"steps"`
	Strategy    struct {
		Matrix struct {
			Include []struct {
				Runner string `yaml:"runner"`
				Target string `yaml:"target"`
				GOOS   string `yaml:"goos"`
				GOARCH string `yaml:"goarch"`
			} `yaml:"include"`
		} `yaml:"matrix"`
	} `yaml:"strategy"`
}

type workflowStep struct {
	ID   string            `yaml:"id"`
	Name string            `yaml:"name"`
	Uses string            `yaml:"uses"`
	Run  string            `yaml:"run"`
	With map[string]any    `yaml:"with"`
	Env  map[string]string `yaml:"env"`
}

func readWorkflow(t *testing.T) workflow {
	t.Helper()
	data, err := os.ReadFile("../../.github/workflows/release.yml")
	if err != nil {
		t.Fatal(err)
	}
	var result workflow
	if err := yaml.Unmarshal(data, &result); err != nil {
		t.Fatal(err)
	}
	return result
}

func TestReleaseWorkflowBoundaries(t *testing.T) {
	w := readWorkflow(t)
	if w.Permissions["contents"] != "read" || len(w.Permissions) != 1 {
		t.Fatalf("build workflow is not read-only: %v", w.Permissions)
	}
	if got := w.On["release"].Types; len(got) != 1 || got[0] != "published" {
		t.Fatalf("published release trigger lost: %v", got)
	}
	if value := w.On["workflow_dispatch"].Inputs["upload_to_draft"].Default; value != false {
		t.Fatalf("manual workflow uploads by default: %v", value)
	}
	for _, name := range []string{"resolve", "build"} {
		assertReadOnlyReleaseJob(t, name, w.Jobs[name])
	}
	assertNativeReleaseBuild(t, w.Jobs["build"])
	assertReleaseUploadJob(t, w.Jobs["upload"])
}

func assertReadOnlyReleaseJob(t *testing.T, name string, job workflowJob) {
	t.Helper()
	if len(job.Permissions) != 0 {
		t.Fatalf("%s overrides read-only permissions", name)
	}
	for _, step := range job.Steps {
		if strings.HasPrefix(step.Uses, "actions/checkout@") {
			if step.With["persist-credentials"] != false {
				t.Fatalf("%s retains a checkout token", name)
			}
			if step.With["ref"] != "${{ github.sha }}" {
				t.Fatalf("%s checks out code outside the triggering workflow revision", name)
			}
		}
		if strings.Contains(step.Run, "${{") {
			t.Fatalf("%s interpolates expressions into shell code", name)
		}
	}
}

func assertNativeReleaseBuild(t *testing.T, build workflowJob) {
	t.Helper()
	matrix := build.Strategy.Matrix.Include
	if len(matrix) != 2 ||
		matrix[0].Runner != "ubuntu-latest" || matrix[0].Target != "linux-x64" || matrix[0].GOOS != "linux" || matrix[0].GOARCH != "amd64" ||
		matrix[1].Runner != "macos-15" || matrix[1].Target != "darwin-arm64" || matrix[1].GOOS != "darwin" || matrix[1].GOARCH != "arm64" {
		t.Fatalf("native release matrix changed: %+v", matrix)
	}
	if build.Steps[0].With["ref"] != "${{ github.sha }}" {
		t.Fatal("builds no longer use the immutable triggering workflow commit")
	}
}

func assertReleaseUploadJob(t *testing.T, upload workflowJob) {
	t.Helper()
	if upload.Permissions["contents"] != "write" || len(upload.Permissions) != 1 ||
		upload.If != "github.event_name == 'release' || inputs.upload_to_draft" {
		t.Fatalf("unexpected upload authority: %+v", upload)
	}
	needs, ok := upload.Needs.([]any)
	if !ok || len(needs) != 2 || needs[0] != "resolve" || needs[1] != "build" {
		t.Fatalf("upload does not wait for both native builds: %v", upload.Needs)
	}
	for _, step := range upload.Steps {
		if strings.HasPrefix(step.Uses, "actions/checkout@") ||
			strings.Contains(step.Run, "scripts/") ||
			strings.Contains(step.Run, "--ignore-missing") ||
			strings.Contains(step.Run, "gh release create") ||
			strings.Contains(step.Run, "gh release edit") {
			t.Fatalf("write job executes repository code or publishes a release: %+v", step)
		}
	}
}

type uploadCase struct {
	name     string
	event    string
	draft    string
	tag      string
	sha      string
	tagType  string
	apiFails bool
	damage   string
	succeeds bool
}

func TestUploadScriptChecksIdentityDraftAndChecksums(t *testing.T) {
	w := readWorkflow(t)
	var script string
	for _, step := range w.Jobs["upload"].Steps {
		if step.Run != "" {
			script = step.Run
		}
	}
	if !strings.Contains(script, "gh release upload") {
		t.Fatal("missing upload transaction")
	}
	for _, tc := range []uploadCase{
		{name: "manual draft", event: "workflow_dispatch", draft: "true", succeeds: true},
		{name: "annotated tag", event: "workflow_dispatch", draft: "true", tagType: "tag", succeeds: true},
		{name: "non-commit ref", event: "workflow_dispatch", draft: "true", tagType: "tree"},
		{name: "extra ref fields", event: "workflow_dispatch", draft: "true", sha: strings.Repeat("a", 40) + " extra"},
		{name: "failed API with misleading stdout", event: "workflow_dispatch", draft: "true", apiFails: true},
		{name: "published", event: "release", draft: "false", succeeds: true},
		{name: "manual published refuses", event: "workflow_dispatch", draft: "false"},
		{name: "published reverted to draft refuses", event: "release", draft: "true"},
		{name: "moved tag", event: "workflow_dispatch", draft: "true", sha: strings.Repeat("b", 40)},
		{name: "malicious tag", event: "workflow_dispatch", draft: "true", tag: "v0.7.0;touch unsafe"},
		{name: "missing release", event: "workflow_dispatch", draft: "missing"},
		{name: "corrupt asset", event: "workflow_dispatch", draft: "true", damage: "corrupt"},
		{name: "missing asset", event: "workflow_dispatch", draft: "true", damage: "missing"},
		{name: "duplicate checksums", event: "workflow_dispatch", draft: "true", damage: "duplicate"},
		{name: "unsafe checksum path", event: "workflow_dispatch", draft: "true", damage: "path"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			runUploadCase(t, script, tc)
		})
	}
}

func runUploadCase(t *testing.T, script string, tc uploadCase) {
	t.Helper()
	root := newUploadFixture(t, tc.damage)
	bin := filepath.Join(root, "bin")
	tag := tc.tag
	if tag == "" {
		tag = "v0.7.0"
	}
	sha := tc.sha
	if sha == "" {
		sha = strings.Repeat("a", 40)
	}
	tagType := tc.tagType
	if tagType == "" {
		tagType = "commit"
	}
	apiFails := "false"
	if tc.apiFails {
		apiFails = "true"
	}
	cmd := exec.Command("bash", "-e", "-o", "pipefail", "-c", script)
	cmd.Dir = root
	cmd.Env = append(os.Environ(),
		"PATH="+bin+string(os.PathListSeparator)+os.Getenv("PATH"),
		"RELEASE_TAG="+tag, "RELEASE_COMMIT="+strings.Repeat("a", 40),
		"RELEASE_EVENT="+tc.event, "GITHUB_REPOSITORY=bingosuite/bingo",
		"GH_TOKEN=test-only", "FAKE_DRAFT="+tc.draft, "FAKE_SHA="+sha,
		"FAKE_TAG_TYPE="+tagType,
		"FAKE_API_FAIL="+apiFails,
		"UPLOAD_LOG="+filepath.Join(root, "uploaded"),
	)
	out, err := cmd.CombinedOutput()
	if (err == nil) != tc.succeeds {
		t.Fatalf("upload outcome mismatch: %v\n%s", err, out)
	}
	uploaded, readErr := os.ReadFile(filepath.Join(root, "uploaded"))
	if tc.succeeds {
		if readErr != nil || !strings.Contains(string(uploaded), "darwin-arm64.vsix") ||
			!strings.Contains(string(uploaded), "linux-x64.vsix") ||
			strings.Count(string(uploaded), "_SHA256SUMS.txt") != 2 {
			t.Fatalf("missing platform assets: %v, %s", readErr, uploaded)
		}
	} else if !os.IsNotExist(readErr) {
		t.Fatalf("failed transaction uploaded assets: %v, %s", readErr, uploaded)
	}
}

func newUploadFixture(t *testing.T, damage string) string {
	t.Helper()
	root := t.TempDir()
	bin := filepath.Join(root, "bin")
	directory := filepath.Join(root, "release-assets")
	for _, dir := range []string{bin, directory} {
		if err := os.Mkdir(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	for _, platform := range []struct {
		name   string
		target string
	}{
		{"linux_amd64", "linux-x64"},
		{"darwin_arm64", "darwin-arm64"},
	} {
		base := "bingo_v0.7.0_" + platform.name
		assets := []string{base + ".tar.gz", base + ".json", "bingo-neovim_v0.7.0_" + platform.name + ".tar.gz", "bingo-v0.7.0-" + platform.target + ".vsix"}
		sums := base + "_SHA256SUMS.txt"
		writeChecksumFixture(t, directory, sums, assets)
		damageUploadFixture(t, directory, sums, assets[0], damage)
	}
	gh := `#!/bin/bash
set -eu
case "$1 $2" in
  "api repos/bingosuite/bingo/git/ref/tags/"*)
    printf '%s\t%s\n' "$FAKE_TAG_TYPE" "$FAKE_SHA"
    [[ "$FAKE_API_FAIL" != true ]] || exit 1
    ;;
  "api repos/bingosuite/bingo/git/tags/"*) printf 'commit\t%s\n' "$FAKE_SHA" ;;
  "release view")
    [[ "$FAKE_DRAFT" != missing ]] || { echo 'release not found' >&2; exit 1; }
    echo "$FAKE_DRAFT"
    ;;
  "release upload") printf '%s\n' "$*" > "$UPLOAD_LOG" ;;
  *) echo "unexpected gh invocation: $*" >&2; exit 1 ;;
esac
`
	if err := os.WriteFile(filepath.Join(bin, "gh"), []byte(gh), 0o755); err != nil {
		t.Fatal(err)
	}
	return root
}

func damageUploadFixture(t *testing.T, directory, sums, asset, kind string) {
	t.Helper()
	var err error
	switch kind {
	case "corrupt":
		err = os.WriteFile(filepath.Join(directory, asset), []byte("corrupted"), 0o644)
	case "missing":
		err = os.Remove(filepath.Join(directory, asset))
	case "duplicate", "path":
		var contents []byte
		contents, err = os.ReadFile(filepath.Join(directory, sums))
		if err == nil {
			if kind == "duplicate" {
				contents = append(contents, contents...)
			} else {
				contents = []byte(strings.ReplaceAll(string(contents), asset, "../escape"))
			}
			err = os.WriteFile(filepath.Join(directory, sums), contents, 0o644)
		}
	}
	if err != nil {
		t.Fatal(err)
	}
}
