// Package release checks the release rules in .github/workflows/publish.yml
// that would otherwise only fail on a real tag push.
package release

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// readRepoFile reads a file relative to the repository root.
func readRepoFile(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "..", name))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// tagRE returns the TAG_RE that publish.yml validates release tags with.
func tagRE(t *testing.T) *regexp.Regexp {
	t.Helper()
	m := regexp.MustCompile(`(?m)^\s*TAG_RE:\s*'([^']+)'`).FindStringSubmatch(readRepoFile(t, ".github/workflows/publish.yml"))
	if m == nil {
		t.Fatal("publish.yml has no TAG_RE")
	}
	return regexp.MustCompile(m[1])
}

func TestReleaseTagRegex(t *testing.T) {
	re := tagRE(t)
	for _, tc := range []struct {
		tag  string
		want bool
	}{
		{"v0.1.0", true},
		{"v1.2.3", true},
		{"v10.20.30", true},
		{"v1.2.3-rc.1", true},
		{"v1.2.3-rc.0", true},
		{"v1.2.3-alpha", true},
		{"v1.2.3-0", true},
		{"v1.2.3-x-y.1a", true},
		{"v1.2.3-0a.1", true},

		{"v1.2.3rc1", false},      // missing hyphen
		{"v1.2.3.4", false},       // four parts
		{"v01.2.3", false},        // leading zero
		{"v1.02.3", false},        // leading zero
		{"v1.2.03", false},        // leading zero
		{"v1.2.3+build.1", false}, // build metadata
		{"v1.2.3-rc.1+b", false},  // build metadata
		{"v1.2.3-rc.01", false},   // leading zero in a numeric identifier
		{"v1.2.3-", false},        // empty pre-release
		{"v1.2.3-rc..1", false},   // empty identifier
		{"v1.2.3-rc_1", false},    // invalid character
		{"v1.2", false},           // does not match the trigger either
		{"1.2.3", false},          // no v
		{"v1.2.3\n", false},
	} {
		if got := re.MatchString(tc.tag); got != tc.want {
			t.Errorf("TAG_RE.MatchString(%q) = %v, want %v", tc.tag, got, tc.want)
		}
	}
}

// strictIdentity returns the single strict cosign identity regex in s: the
// one that names publish.yml.
func strictIdentity(t *testing.T, name string) string {
	t.Helper()
	var found []string
	for _, m := range regexp.MustCompile(`--certificate-identity-regexp '([^']+)'`).FindAllStringSubmatch(readRepoFile(t, name), -1) {
		if strings.Contains(m[1], `publish\.yml`) {
			found = append(found, m[1])
		}
	}
	if len(found) != 1 {
		t.Fatalf("%s: want exactly one strict identity regex, found %d", name, len(found))
	}
	return found[0]
}

func TestStrictIdentityRegex(t *testing.T) {
	workflow := strictIdentity(t, ".github/workflows/publish.yml")
	readme := strictIdentity(t, "README.md")
	if workflow != readme {
		t.Fatalf("publish.yml and README.md strict identity regexes differ:\n%s\n%s", workflow, readme)
	}

	re := regexp.MustCompile(workflow)
	const prefix = "https://github.com/jackspiering/bop/.github/workflows/publish.yml@"
	tags := tagRE(t)
	for _, tc := range []struct {
		ref  string
		want bool
	}{
		{"refs/heads/main", true},
		{"refs/tags/v0.1.0", true},
		{"refs/tags/v1.2.3-rc.1", true},
		{"refs/heads/feature", false},
		{"refs/pull/1/merge", false},
		{"refs/tags/v1.2.3+build.1", false},
		{"refs/tags/v1.2.3rc1", false},
		{"refs/tags/v1.2.3/x", false},
	} {
		if got := re.MatchString(prefix + tc.ref); got != tc.want {
			t.Errorf("identity regex on %q = %v, want %v", tc.ref, got, tc.want)
		}
		// Every tag the workflow accepts must also pass verification.
		if name, ok := strings.CutPrefix(tc.ref, "refs/tags/"); ok && tags.MatchString(name) && !re.MatchString(prefix+tc.ref) {
			t.Errorf("TAG_RE accepts %q but the identity regex rejects it", name)
		}
	}
	if re.MatchString("https://github.com/jackspiering/bop/.github/workflows/ci.yml@refs/heads/main") {
		t.Error("identity regex accepts ci.yml")
	}
}
