package release

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// gitRepo is a scratch repository with a linear main history.
type gitRepo struct {
	t   *testing.T
	dir string
}

func newGitRepo(t *testing.T) *gitRepo {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	r := &gitRepo{t: t, dir: t.TempDir()}
	r.git("init", "-q", "-b", "main")
	return r
}

func (r *gitRepo) git(args ...string) string {
	r.t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = r.dir
	cmd.Env = append(os.Environ(),
		"GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1",
		"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.com",
		"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.com")
	out, err := cmd.CombinedOutput()
	if err != nil {
		r.t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return strings.TrimSpace(string(out))
}

// commit adds an empty commit to main, moves origin/main to it and returns
// its SHA.
func (r *gitRepo) commit() string {
	r.t.Helper()
	r.git("commit", "-q", "--allow-empty", "-m", "c")
	sha := r.git("rev-parse", "HEAD")
	r.git("update-ref", "refs/remotes/origin/main", sha)
	return sha
}

// movingTags runs .github/scripts/moving-tags.sh in the scratch repository.
func (r *gitRepo) movingTags(refType, refName, sha string) string {
	r.t.Helper()
	script, err := filepath.Abs(filepath.Join("..", "..", ".github", "scripts", "moving-tags.sh"))
	if err != nil {
		r.t.Fatal(err)
	}
	cmd := exec.Command("bash", script)
	cmd.Dir = r.dir
	cmd.Env = append(os.Environ(), "GITHUB_REF_TYPE="+refType, "GITHUB_REF_NAME="+refName, "GITHUB_SHA="+sha)
	out, err := cmd.CombinedOutput()
	if err != nil {
		r.t.Fatalf("moving-tags.sh %s %s: %v\n%s", refType, refName, err, out)
	}
	return strings.Join(strings.Fields(string(out)), " ")
}

func TestMovingTagsReleases(t *testing.T) {
	r := newGitRepo(t)
	c1, c2, c3 := r.commit(), r.commit(), r.commit()
	r.git("tag", "v0.1.0", c1)
	r.git("tag", "v0.2.0", c3)
	r.git("tag", "v0.1.1", c2) // tagged after v0.2.0, on an older commit
	r.git("tag", "v0.1.2", c2)
	r.git("tag", "v0.3.0-rc.1", c3)
	r.git("tag", "v0.10.0-rc.1", c3)

	for _, tc := range []struct {
		tag, want string
	}{
		{"v0.2.0", "latest=true minor=true edge=false"},        // highest stable
		{"v0.1.0", "latest=false minor=false edge=false"},      // re-run of an old release
		{"v0.1.1", "latest=false minor=false edge=false"},      // superseded by v0.1.2
		{"v0.1.2", "latest=false minor=true edge=false"},       // newest patch of 0.1 only
		{"v0.3.0-rc.1", "latest=false minor=false edge=false"}, // pre-release: exact version only
	} {
		if got := r.movingTags("tag", tc.tag, ""); got != tc.want {
			t.Errorf("%s: got %q, want %q", tc.tag, got, tc.want)
		}
	}

	// Versions sort numerically, not as strings, and pre-releases never count.
	r.git("tag", "v0.10.0", c3)
	r.git("tag", "v0.9.0", c3)
	if got, want := r.movingTags("tag", "v0.10.0", ""), "latest=true minor=true edge=false"; got != want {
		t.Errorf("v0.10.0: got %q, want %q", got, want)
	}
	if got, want := r.movingTags("tag", "v0.9.0", ""), "latest=false minor=true edge=false"; got != want {
		t.Errorf("v0.9.0: got %q, want %q", got, want)
	}
}

func TestMovingTagsMain(t *testing.T) {
	r := newGitRepo(t)
	old := r.commit()
	tip := r.commit()
	if got, want := r.movingTags("branch", "main", tip), "latest=false minor=false edge=true"; got != want {
		t.Errorf("tip of main: got %q, want %q", got, want)
	}
	if got, want := r.movingTags("branch", "main", old), "latest=false minor=false edge=false"; got != want {
		t.Errorf("re-run of an older main commit: got %q, want %q", got, want)
	}
}
