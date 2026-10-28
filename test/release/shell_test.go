package release

import (
	"path/filepath"
	"regexp"
	"testing"
)

// TestPublishRunsWithPipefail: a step that pipes a script into tee only fails
// the run if bash runs with pipefail, which needs an explicit shell: bash.
func TestPublishRunsWithPipefail(t *testing.T) {
	wf := readRepoFile(t, filepath.Join(".github", "workflows", "publish.yml"))
	if !regexp.MustCompile(`(?m)^defaults:\s*\n\s+run:\s*\n\s+shell:\s*bash\s*$`).MatchString(wf) {
		t.Error("publish.yml must set defaults.run.shell: bash so piped steps get pipefail")
	}
}
