package release

import (
	"regexp"
	"testing"
)

// TestGoVersionsAgree: CI tests and scans with the go.mod toolchain, but the
// image is built with the Dockerfile's golang tag, so the two must match.
func TestGoVersionsAgree(t *testing.T) {
	dockerfile := regexp.MustCompile(`(?m)^FROM\s+(?:--\S+\s+)*golang:([0-9]+\.[0-9]+\.[0-9]+)`).FindStringSubmatch(readRepoFile(t, "Dockerfile"))
	if dockerfile == nil {
		t.Fatal("Dockerfile has no FROM golang:<version> line")
	}
	gomod := regexp.MustCompile(`(?m)^go\s+([0-9]+\.[0-9]+\.[0-9]+)\s*$`).FindStringSubmatch(readRepoFile(t, "go.mod"))
	if gomod == nil {
		t.Fatal("go.mod has no go <version> line")
	}
	if dockerfile[1] != gomod[1] {
		t.Errorf("Dockerfile builds with golang:%s but go.mod says go %s; bump both in the same PR", dockerfile[1], gomod[1])
	}
}
