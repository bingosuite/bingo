package main

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

func releaseCommitStep(t *testing.T) workflowStep {
	t.Helper()
	for _, step := range readWorkflow(t).Jobs["resolve"].Steps {
		if step.ID == "commit" {
			return step
		}
	}
	t.Fatal("missing release context guard")
	return workflowStep{}
}

func TestReleaseContextBindings(t *testing.T) {
	resolve := readWorkflow(t).Jobs["resolve"]
	if resolve.Steps[0].With["fetch-depth"] != 0 {
		t.Fatal("release context guard must resolve tags without checking out their code")
	}
	step := releaseCommitStep(t)
	if step.Env["RELEASE_COMMIT"] != "${{ github.sha }}" ||
		step.Env["RELEASE_TAG"] != "${{ steps.tag.outputs.tag }}" {
		t.Fatalf("release context guard is not bound to the trigger and validated tag: %v", step.Env)
	}
}

type releaseContextCase struct {
	name          string
	tagOther      bool
	checkoutOther bool
	triggerOther  bool
	annotated     bool
	missing       bool
	succeeds      bool
}

func TestReleaseContextRejectsIndependentTagCode(t *testing.T) {
	for _, tc := range []releaseContextCase{
		{name: "matching historical tag context", succeeds: true},
		{name: "matching annotated tag context", annotated: true, succeeds: true},
		{name: "matching current revision", tagOther: true, checkoutOther: true, triggerOther: true, succeeds: true},
		{name: "valid tag at another commit", tagOther: true},
		{name: "annotated tag at another commit", tagOther: true, annotated: true},
		{name: "input tag checkout cannot replace trigger", tagOther: true, checkoutOther: true},
		{name: "checkout differs from trigger", checkoutOther: true},
		{name: "tag and checkout differ from trigger", triggerOther: true},
		{name: "missing tag", missing: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			runReleaseContextCase(t, tc)
		})
	}
}

func runReleaseContextCase(t *testing.T, tc releaseContextCase) {
	t.Helper()
	repo := newReleaseContextRepo(t)
	tagCommit, checkout, trigger := repo.trusted, repo.trusted, repo.trusted
	if tc.tagOther {
		tagCommit = repo.other
	}
	if tc.checkoutOther {
		checkout = repo.other
	}
	if tc.triggerOther {
		trigger = repo.other
	}
	if !tc.missing {
		args := []string{"tag", "v0.7.0", tagCommit}
		if tc.annotated {
			args = append(args, "-a", "-m", "release")
		}
		repo.git(t, args...)
	}
	repo.git(t, "branch", "v0.7.0", trigger)
	repo.git(t, "checkout", "-q", "--detach", checkout)

	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	output := filepath.Join(t.TempDir(), "output")
	cmd := exec.CommandContext(ctx, "bash", "-e", "-o", "pipefail", "-c", releaseCommitStep(t).Run)
	cmd.Dir = repo.root
	cmd.Env = append(os.Environ(),
		"RELEASE_TAG=v0.7.0",
		"RELEASE_COMMIT="+trigger,
		"GITHUB_OUTPUT="+output,
	)
	out, err := cmd.CombinedOutput()
	if ctx.Err() != nil {
		t.Fatalf("release context guard did not complete: %v\n%s", ctx.Err(), out)
	}
	if (err == nil) != tc.succeeds {
		t.Fatalf("release context outcome mismatch: %v\n%s", err, out)
	}
	values, readErr := os.ReadFile(output)
	if readErr != nil && !os.IsNotExist(readErr) {
		t.Fatal(readErr)
	}
	if tc.succeeds {
		if string(values) != "commit="+trigger+"\n" {
			t.Fatalf("release identity does not match the trigger: %q", values)
		}
	} else if len(values) != 0 {
		t.Fatalf("rejected context published a release identity: %q", values)
	}
}

type releaseContextRepo struct {
	root    string
	trusted string
	other   string
}

func newReleaseContextRepo(t *testing.T) releaseContextRepo {
	t.Helper()
	repo := releaseContextRepo{root: t.TempDir()}
	repo.git(t, "init", "-q", "--object-format=sha1")
	repo.git(t, "commit", "--allow-empty", "-qm", "trusted workflow revision")
	repo.trusted = repo.git(t, "rev-parse", "HEAD")
	repo.git(t, "commit", "--allow-empty", "-qm", "independently selected tag code")
	repo.other = repo.git(t, "rev-parse", "HEAD")
	return repo
}

func (r releaseContextRepo) git(t *testing.T, args ...string) string {
	t.Helper()
	config := []string{
		"-c", "user.name=Release Context Test",
		"-c", "user.email=test@example.invalid",
		"-c", "commit.gpgsign=false",
		"-c", "tag.gpgsign=false",
		"-c", "core.hooksPath=/dev/null",
	}
	value, err := capture(r.root, nil, "git", append(config, args...)...)
	if err != nil {
		t.Fatal(err)
	}
	return value
}
