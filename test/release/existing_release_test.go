package release

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

const (
	existingImage  = "ghcr.io/example/bop"
	existingDigest = "sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	existingSHA    = "abcdef0123456789abcdef0123456789abcdef01"
)

// runExistingRelease runs .github/scripts/existing-release.sh with a fake
// docker first on PATH. The fake prints dockerOut and exits with dockerCode.
// It returns the combined output, the contents of GITHUB_OUTPUT and the exit
// error (nil on success).
func runExistingRelease(t *testing.T, env []string, dockerOut string, dockerCode int) (out, ghOutput string, err error) {
	t.Helper()
	script, absErr := filepath.Abs(filepath.Join("..", "..", ".github", "scripts", "existing-release.sh"))
	if absErr != nil {
		t.Fatal(absErr)
	}
	dir := t.TempDir()
	bin := filepath.Join(dir, "bin")
	if mkErr := os.Mkdir(bin, 0o755); mkErr != nil {
		t.Fatal(mkErr)
	}
	fake := fmt.Sprintf("#!/bin/sh\ncat <<'EOF'\n%s\nEOF\nexit %d\n", dockerOut, dockerCode)
	if wErr := os.WriteFile(filepath.Join(bin, "docker"), []byte(fake), 0o755); wErr != nil {
		t.Fatal(wErr)
	}
	ghFile := filepath.Join(dir, "github_output")
	if wErr := os.WriteFile(ghFile, nil, 0o644); wErr != nil {
		t.Fatal(wErr)
	}
	cmd := exec.Command("bash", script)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(),
		"PATH="+bin+string(os.PathListSeparator)+os.Getenv("PATH"),
		"IMAGE="+existingImage,
		"GITHUB_SHA="+existingSHA,
		"GITHUB_OUTPUT="+ghFile)
	cmd.Env = append(cmd.Env, env...)
	b, err := cmd.CombinedOutput()
	got, readErr := os.ReadFile(ghFile)
	if readErr != nil {
		t.Fatal(readErr)
	}
	return string(b), string(got), err
}

func TestExistingRelease(t *testing.T) {
	tagEnv := func(attempt string) []string {
		return []string{"GITHUB_REF_TYPE=tag", "GITHUB_REF_NAME=v1.2.3", "GITHUB_REF=refs/tags/v1.2.3", "GITHUB_RUN_ATTEMPT=" + attempt}
	}
	mainEnv := func(attempt string) []string {
		return []string{"GITHUB_REF_TYPE=branch", "GITHUB_REF_NAME=main", "GITHUB_REF=refs/heads/main", "GITHUB_RUN_ATTEMPT=" + attempt}
	}
	tagRef := existingImage + ":1.2.3"
	shaRef := existingImage + ":sha-abcdef0"

	for _, tc := range []struct {
		name       string
		env        []string
		dockerOut  string
		dockerCode int
		wantFail   bool
		wantDigest string // expected digest= value; empty means no output line
		wantInOut  string // substring of the script's own output
	}{
		{
			name: "tag not found: build proceeds", env: tagEnv("1"),
			dockerOut: "ERROR: " + tagRef + ": not found", dockerCode: 1,
		},
		{
			name: "tag found on attempt 1: immutable, fails", env: tagEnv("1"),
			dockerOut: existingDigest, wantFail: true, wantInOut: "release tags are immutable",
		},
		{
			name: "tag found on a later attempt: reuses the digest", env: tagEnv("2"),
			dockerOut: existingDigest, wantDigest: existingDigest,
		},
		{
			name: "tag inspect fails with other text", env: tagEnv("2"),
			dockerOut: "ERROR: unauthorized", dockerCode: 1, wantFail: true, wantInOut: "cannot check " + tagRef,
		},
		{
			name: "tag found but the digest is malformed", env: tagEnv("2"),
			dockerOut: "sha256:nothex", wantFail: true, wantInOut: "cannot read the digest of " + tagRef,
		},
		{
			name: "tag not found for another ref is not a match", env: tagEnv("1"),
			dockerOut: "ERROR: " + existingImage + ":9.9.9: not found", dockerCode: 1, wantFail: true,
		},
		{
			name: "main: sha tag not found, build proceeds", env: mainEnv("1"),
			dockerOut: "ERROR: " + shaRef + ": not found", dockerCode: 1,
		},
		{
			name: "main: sha tag found on attempt 1 reuses the digest", env: mainEnv("1"),
			dockerOut: existingDigest, wantDigest: existingDigest,
		},
		{
			name: "main: sha tag found on a later attempt reuses the digest", env: mainEnv("3"),
			dockerOut: existingDigest, wantDigest: existingDigest,
		},
		{
			name: "main: inspect fails with other text", env: mainEnv("1"),
			dockerOut: "ERROR: timeout", dockerCode: 1, wantFail: true, wantInOut: "cannot check " + shaRef,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out, ghOutput, err := runExistingRelease(t, tc.env, tc.dockerOut, tc.dockerCode)
			if tc.wantFail && err == nil {
				t.Errorf("want a non-zero exit, got success\n%s", out)
			}
			if !tc.wantFail && err != nil {
				t.Errorf("want success, got %v\n%s", err, out)
			}
			if tc.wantInOut != "" && !strings.Contains(out, tc.wantInOut) {
				t.Errorf("output lacks %q:\n%s", tc.wantInOut, out)
			}
			want := ""
			if tc.wantDigest != "" {
				want = "digest=" + tc.wantDigest + "\n"
			}
			if ghOutput != want {
				t.Errorf("GITHUB_OUTPUT = %q, want %q", ghOutput, want)
			}
		})
	}
}
