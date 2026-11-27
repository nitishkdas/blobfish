package webui

import (
	"io/fs"
	"path"
	"regexp"
	"strings"
	"testing"
)

// The UI has no build step, so a module that calls another module's
// function without importing it only fails when that code path runs in a
// browser (node --check can't see it). Catch it here: every exported
// function a module calls must be imported or defined by that module.
func TestModulesImportWhatTheyCall(t *testing.T) {
	exportRe := regexp.MustCompile(`(?m)^export\s+(?:async\s+)?function\s+(\w+)`)
	importRe := regexp.MustCompile(`import\s*\{([^}]*)\}\s*from`)
	defRe := regexp.MustCompile(`(?:function|const|let|var|class)\s+(\w+)`)

	srcs := map[string]string{}
	exportedBy := map[string]string{}
	matches, err := fs.Glob(files, "static/js/*.js")
	if err != nil || len(matches) == 0 {
		t.Fatalf("no modules found: %v", err)
	}
	for _, p := range matches {
		b, err := fs.ReadFile(files, p)
		if err != nil {
			t.Fatal(err)
		}
		srcs[p] = string(b)
		for _, m := range exportRe.FindAllStringSubmatch(string(b), -1) {
			exportedBy[m[1]] = p
		}
	}
	for p, src := range srcs {
		known := map[string]bool{}
		for _, m := range importRe.FindAllStringSubmatch(src, -1) {
			for name := range strings.SplitSeq(m[1], ",") {
				known[strings.TrimSpace(name)] = true
			}
		}
		for _, m := range defRe.FindAllStringSubmatch(src, -1) {
			known[m[1]] = true
		}
		for name, from := range exportedBy {
			if from == p || known[name] {
				continue
			}
			if regexp.MustCompile(`(?:^|[^\w.])` + name + `\s*\(`).MatchString(src) {
				t.Errorf("%s calls %s() from %s without importing it", path.Base(p), name, path.Base(from))
			}
		}
	}
}
