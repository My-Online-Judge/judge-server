package testutil

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/My-Online-Judge/judge-server/internal/bundle"
	"github.com/My-Online-Judge/judge-server/internal/compare"
)

// WriteBundle builds the bundle <root>/<id> on disk the way
// TestCaseBundleBuilder writes it: an "info" file with the Java shape (the
// stripped md5 of each expected output) plus one <name>.in and <name>.out per
// case. A non-empty spjVersion marks the bundle as using a checker. It returns
// the bundle as bundle.Load reads it back. id must be "<slug>__<hash>".
func WriteBundle(t testing.TB, root, id string, cases []struct{ Name, In, Out string }, spjVersion string) *bundle.Bundle {
	t.Helper()
	dir := filepath.Join(root, id)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("testutil: mkdir bundle: %v", err)
	}
	testCases := make(map[string]any, len(cases))
	for _, c := range cases {
		write := func(name, data string) {
			if err := os.WriteFile(filepath.Join(dir, name), []byte(data), 0o644); err != nil {
				t.Fatalf("testutil: write %s: %v", name, err)
			}
		}
		write(c.Name+".in", c.In)
		write(c.Name+".out", c.Out)
		testCases[c.Name] = map[string]any{
			"input_name":          c.Name + ".in",
			"input_size":          len(c.In),
			"output_name":         c.Name + ".out",
			"output_size":         len(c.Out),
			"output_md5":          compare.MD5Hex([]byte(c.Out)),
			"stripped_output_md5": compare.MD5Hex(compare.Normalize([]byte(c.Out))),
		}
	}
	info := map[string]any{
		"spj":              spjVersion != "",
		"test_case_number": len(cases),
		"test_cases":       testCases,
	}
	if spjVersion != "" {
		info["spj_version"] = spjVersion
	}
	raw, err := json.Marshal(info)
	if err != nil {
		t.Fatalf("testutil: marshal info: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "info"), raw, 0o644); err != nil {
		t.Fatalf("testutil: write info: %v", err)
	}
	b, err := bundle.Load(root, id)
	if err != nil {
		t.Fatalf("testutil: load bundle %s: %v", id, err)
	}
	return b
}
