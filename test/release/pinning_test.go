package release

import (
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

var (
	buildkitPin = regexp.MustCompile(`^image=moby/buildkit:v[0-9.]+@sha256:[0-9a-f]{64}$`)
	sbomPin     = regexp.MustCompile(`^generator=docker/buildkit-syft-scanner:[0-9.]+@sha256:[0-9a-f]{64}$`)
	keyValue    = regexp.MustCompile(`^\s*([a-z-]+):\s*(.*?)\s*$`)
)

func indent(line string) int { return len(line) - len(strings.TrimLeft(line, " ")) }

// stepBlocks returns, for each step whose first line contains marker, the
// lines that belong to that step.
func stepBlocks(src, marker string) [][]string {
	lines := strings.Split(src, "\n")
	var blocks [][]string
	for i, l := range lines {
		if !strings.Contains(l, marker) || !strings.HasPrefix(strings.TrimSpace(l), "- ") {
			continue
		}
		block := []string{l}
		for _, next := range lines[i+1:] {
			if strings.TrimSpace(next) != "" && indent(next) <= indent(l) {
				break
			}
			block = append(block, next)
		}
		blocks = append(blocks, block)
	}
	return blocks
}

// TestBuildToolsPinned checks that every workflow pins the BuildKit daemon
// and the SBOM generator by digest. Dependabot does not track either.
func TestBuildToolsPinned(t *testing.T) {
	files, err := filepath.Glob(filepath.Join("..", "..", ".github", "workflows", "*.yml"))
	if err != nil || len(files) == 0 {
		t.Fatalf("no workflows found: %v", err)
	}
	buildkit := map[string]string{} // image reference -> first file using it
	uses := 0
	for _, f := range files {
		name := filepath.Base(f)
		src := readRepoFile(t, filepath.Join(".github", "workflows", name))
		for _, block := range stepBlocks(src, "uses: docker/setup-buildx-action@") {
			uses++
			var image string
			for _, l := range block {
				if m := keyValue.FindStringSubmatch(l); m != nil && m[1] == "driver-opts" {
					image = m[2]
				}
			}
			if !buildkitPin.MatchString(image) {
				t.Errorf("%s: setup-buildx-action without a digest-pinned driver-opts image (got %q)", name, image)
				continue
			}
			buildkit[image] = name
		}
		for _, l := range strings.Split(src, "\n") {
			if m := keyValue.FindStringSubmatch(l); m != nil && m[1] == "sbom" && !sbomPin.MatchString(m[2]) {
				t.Errorf("%s: sbom must name a digest-pinned generator, got %q", name, m[2])
			}
		}
	}
	if uses == 0 {
		t.Error("no setup-buildx-action uses found")
	}
	if len(buildkit) > 1 {
		t.Errorf("workflows pin different BuildKit images: %v", buildkit)
	}
}
