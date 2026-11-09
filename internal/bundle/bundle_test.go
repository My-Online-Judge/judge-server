package bundle

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

const (
	md5One = "c4ca4238a0b923820dcc509a6f75849b"
	md5Two = "c81e728d9d4c2f636f067f89cc14862c"
	testID = "two-sum__abc123de"
)

// writeBundle creates root/id with the given files; a non-empty infoJSON is
// written as root/id/info.
func writeBundle(t *testing.T, root, id, infoJSON string, files map[string][]byte) string {
	t.Helper()
	dir := filepath.Join(root, id)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if infoJSON != "" {
		if err := os.WriteFile(filepath.Join(dir, "info"), []byte(infoJSON), 0o644); err != nil {
			t.Fatalf("write info: %v", err)
		}
	}
	for name, data := range files {
		if err := os.WriteFile(filepath.Join(dir, name), data, 0o644); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}
	return dir
}

// infoOf renders an info file with one case per name (input "<name>.in").
func infoOf(extra string, names ...string) string {
	cases := make([]string, len(names))
	for i, n := range names {
		cases[i] = fmt.Sprintf(`%q: {"input_name": "%s.in", "input_size": 2, "output_name": "%s.out", "output_size": 2, "output_md5": "%s", "stripped_output_md5": "%s"}`, n, n, n, md5Two, md5One)
	}
	return fmt.Sprintf(`{"test_case_number": %d, %s"test_cases": {%s}}`, len(names), extra, strings.Join(cases, ","))
}

func inputsOf(names ...string) map[string][]byte {
	m := map[string][]byte{}
	for _, n := range names {
		m[n+".in"] = []byte("in " + n + "\n")
	}
	return m
}

func namesOf(b *Bundle) []string {
	out := make([]string, len(b.Cases))
	for i, c := range b.Cases {
		out[i] = c.Name
	}
	return out
}

func TestValidateID(t *testing.T) {
	ok := []string{
		testID,
		"A1__0123456789abcdef",
		"a.b_c-d__abcdef12",
		"a__b__abcdef12", // the last "__" separates the hash, as rpartition does
		"x__" + strings.Repeat("a", 128),
	}
	for _, id := range ok {
		if err := ValidateID(id); err != nil {
			t.Errorf("ValidateID(%q) = %v, want nil", id, err)
		}
	}
	bad := []string{
		"",
		"../x__abc",
		"../x__abcdef12",
		"a__b/../c",
		"a/b__abcdef12",
		`a\b__abcdef12`,
		"noSeparator",
		"slug__XYZ",
		"slug__ABCDEF12",
		"slug__abc",    // hash shorter than 8
		"__abcdef12",   // empty slug
		"-x__abcdef12", // slug must start alphanumeric
		".x__abcdef12",
		"x__abcdef12\n",
		"x__abcdef12/",
		"x__" + strings.Repeat("a", 129),
		"x__abcdef12__zz", // the part after the last "__" is not hex
	}
	for _, id := range bad {
		err := ValidateID(id)
		if !errors.Is(err, ErrInvalidID) {
			t.Errorf("ValidateID(%q) = %v, want ErrInvalidID", id, err)
		}
	}
}

func TestLoadReadsCases(t *testing.T) {
	root := t.TempDir()
	dir := writeBundle(t, root, testID, infoOf("", "1", "2"), inputsOf("1", "2"))

	b, err := Load(root, testID)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	realDir, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatal(err)
	}
	if b.Dir != realDir {
		t.Errorf("Dir = %q, want %q", b.Dir, realDir)
	}
	if b.SPJ || b.SPJVersion != "" {
		t.Errorf("SPJ = %v %q, want false and empty", b.SPJ, b.SPJVersion)
	}
	want := []Case{
		{Name: "1", InputPath: filepath.Join(realDir, "1.in"), StrippedOutputMD5: md5One},
		{Name: "2", InputPath: filepath.Join(realDir, "2.in"), StrippedOutputMD5: md5One},
	}
	if !reflect.DeepEqual(b.Cases, want) {
		t.Errorf("Cases = %+v, want %+v", b.Cases, want)
	}
	for _, c := range b.Cases {
		if c.InputBytes != nil {
			t.Errorf("case %s: InputBytes = %q, want nil for a bundle case", c.Name, c.InputBytes)
		}
	}
}

func TestLoadOrdersNumerically(t *testing.T) {
	root := t.TempDir()
	writeBundle(t, root, testID, infoOf("", "10", "2", "1"), inputsOf("10", "2", "1"))
	b, err := Load(root, testID)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got, want := namesOf(b), []string{"1", "2", "10"}; !reflect.DeepEqual(got, want) {
		t.Errorf("order = %v, want %v", got, want)
	}
}

func TestLoadLexicographicWhenNotAllNumeric(t *testing.T) {
	root := t.TempDir()
	names := []string{"b", "10", "a", "2"}
	writeBundle(t, root, testID, infoOf("", names...), inputsOf(names...))
	b, err := Load(root, testID)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got, want := namesOf(b), []string{"10", "2", "a", "b"}; !reflect.DeepEqual(got, want) {
		t.Errorf("order = %v, want %v", got, want)
	}
}

func TestLoadReadsSPJ(t *testing.T) {
	root := t.TempDir()
	writeBundle(t, root, testID, infoOf(`"spj": true, "spj_version": "v1", `, "1"), inputsOf("1"))
	b, err := Load(root, testID)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !b.SPJ || b.SPJVersion != "v1" {
		t.Errorf("SPJ = %v %q, want true v1", b.SPJ, b.SPJVersion)
	}
}

func TestLoadNotFound(t *testing.T) {
	root := t.TempDir()
	writeBundle(t, root, "noinfo__abcdef12", "", inputsOf("1"))
	file := filepath.Join(root, "afile__abcdef12")
	if err := os.WriteFile(file, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	tests := []struct{ name, root, id string }{
		{"missing dir", root, "missing__abcdef12"},
		{"dir without info", root, "noinfo__abcdef12"},
		{"id is a file", root, "afile__abcdef12"},
		{"missing root", filepath.Join(root, "nope"), testID},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			b, err := Load(tt.root, tt.id)
			if !errors.Is(err, ErrNotFound) {
				t.Errorf("Load = %v, %v, want ErrNotFound", b, err)
			}
		})
	}
}

func TestLoadCorrupt(t *testing.T) {
	// plant creates the file a bad name would point at, so that only the name
	// check, not a missing file, can make Load fail.
	plant := func(rel string) func(t *testing.T, dir string) {
		return func(t *testing.T, dir string) {
			t.Helper()
			path := filepath.Join(dir, rel)
			if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, []byte("planted\n"), 0o644); err != nil {
				t.Fatal(err)
			}
		}
	}
	tests := []struct {
		name  string
		info  string
		files map[string][]byte
		setup func(t *testing.T, dir string)
	}{
		{name: "count mismatch", info: strings.Replace(infoOf("", "1", "2"), `"test_case_number": 2`, `"test_case_number": 3`, 1), files: inputsOf("1", "2")},
		{name: "missing 3.in", info: infoOf("", "1", "2", "3"), files: inputsOf("1", "2")},
		{name: "bad JSON", info: `{"test_case_number": 1,`, files: inputsOf("1")},
		{name: "not an object", info: `[1, 2]`, files: inputsOf("1")},
		{name: "no cases", info: `{"test_case_number": 0, "test_cases": {}}`},
		{name: "md5 too short", info: strings.Replace(infoOf("", "1"), md5One, "abc123", 1), files: inputsOf("1")},
		{name: "md5 uppercase", info: strings.Replace(infoOf("", "1"), md5One, strings.ToUpper(md5One), 1), files: inputsOf("1")},
		{name: "md5 not hex", info: strings.Replace(infoOf("", "1"), md5One, strings.Repeat("g", 32), 1), files: inputsOf("1")},
		{name: "md5 missing", info: `{"test_case_number": 1, "test_cases": {"1": {"input_name": "1.in"}}}`, files: inputsOf("1")},
		{name: "input_name missing", info: fmt.Sprintf(`{"test_case_number": 1, "test_cases": {"1": {"stripped_output_md5": "%s"}}}`, md5One), files: inputsOf("1")},
		{name: "input_name climbs out", info: strings.Replace(infoOf("", "1"), `"1.in"`, `"../1.in"`, 1), setup: plant("../1.in")},
		{name: "input_name absolute", info: strings.Replace(infoOf("", "1"), `"1.in"`, `"/etc/passwd"`, 1), setup: plant("etc/passwd")},
		{name: "input_name nested", info: strings.Replace(infoOf("", "1"), `"1.in"`, `"sub/1.in"`, 1), setup: plant("sub/1.in")},
		{name: "input_name dot dot", info: strings.Replace(infoOf("", "1"), `"1.in"`, `".."`, 1)},
		{name: "case name has a slash", info: strings.Replace(infoOf("", "a/b"), `"a/b.in"`, `"x.in"`, 1), setup: plant("x.in")},
		{name: "input is a directory", info: infoOf("", "1"), setup: func(t *testing.T, dir string) {
			t.Helper()
			if err := os.Mkdir(filepath.Join(dir, "1.in"), 0o755); err != nil {
				t.Fatal(err)
			}
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			root := t.TempDir()
			dir := writeBundle(t, root, testID, tt.info, tt.files)
			if tt.setup != nil {
				tt.setup(t, dir)
			}
			b, err := Load(root, testID)
			if !errors.Is(err, ErrCorrupt) {
				t.Errorf("Load = %v, %v, want ErrCorrupt", b, err)
			}
		})
	}
}

func TestLoadCorruptWhenInputIsASymlink(t *testing.T) {
	root := t.TempDir()
	dir := writeBundle(t, root, testID, infoOf("", "1"), nil)
	secret := filepath.Join(t.TempDir(), "secret")
	if err := os.WriteFile(secret, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(secret, filepath.Join(dir, "1.in")); err != nil {
		t.Fatal(err)
	}
	if b, err := Load(root, testID); !errors.Is(err, ErrCorrupt) {
		t.Errorf("Load = %v, %v, want ErrCorrupt", b, err)
	}
}

func TestLoadRefusesInvalidID(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, "root")
	if err := os.Mkdir(root, 0o755); err != nil {
		t.Fatal(err)
	}
	// A valid bundle next to the root, reachable by "../" if the id were joined unchecked.
	writeBundle(t, base, "esc__abcdef12", infoOf("", "1"), inputsOf("1"))
	for _, id := range []string{"../esc__abcdef12", "root/../esc__abcdef12", "", "noSeparator"} {
		if b, err := Load(root, id); !errors.Is(err, ErrInvalidID) {
			t.Errorf("Load(%q) = %v, %v, want ErrInvalidID", id, b, err)
		}
	}
}

func TestLoadRefusesSymlinkLeavingTheRoot(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	writeBundle(t, outside, "real__abcdef12", infoOf("", "1"), inputsOf("1"))
	if err := os.Symlink(filepath.Join(outside, "real__abcdef12"), filepath.Join(root, "evil__abcdef12")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(root, filepath.Join(root, "self__abcdef12")); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"evil__abcdef12", "self__abcdef12"} {
		if b, err := Load(root, id); !errors.Is(err, ErrInvalidID) {
			t.Errorf("Load(%q) = %v, %v, want ErrInvalidID", id, b, err)
		}
	}
}

func TestLoadAllowsSymlinkInsideTheRoot(t *testing.T) {
	root := t.TempDir()
	writeBundle(t, root, "real__abcdef12", infoOf("", "1"), inputsOf("1"))
	if err := os.Symlink("real__abcdef12", filepath.Join(root, "alias__abcdef12")); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(root, "alias__abcdef12"); err != nil {
		t.Errorf("Load through a link inside root: %v", err)
	}
}

func TestLoadThroughSymlinkedRoot(t *testing.T) {
	real := t.TempDir()
	writeBundle(t, real, testID, infoOf("", "1"), inputsOf("1"))
	link := filepath.Join(t.TempDir(), "root")
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}
	b, err := Load(link, testID)
	if err != nil {
		t.Fatalf("Load through a symlinked root: %v", err)
	}
	if len(b.Cases) != 1 {
		t.Errorf("Cases = %d, want 1", len(b.Cases))
	}
}

func TestInlineMD5(t *testing.T) {
	b := Inline([]InlineCase{
		{Input: []byte("1\n"), Output: []byte("1 \r\n")},
		{Input: []byte("2\n"), Output: []byte("2\n")},
	})
	if b.Dir != "" || b.SPJ || b.SPJVersion != "" {
		t.Errorf("Inline bundle has Dir=%q SPJ=%v SPJVersion=%q, want zero values", b.Dir, b.SPJ, b.SPJVersion)
	}
	want := []Case{
		{Name: "1", StrippedOutputMD5: md5One, InputBytes: []byte("1\n")},
		{Name: "2", StrippedOutputMD5: md5Two, InputBytes: []byte("2\n")},
	}
	if !reflect.DeepEqual(b.Cases, want) {
		t.Errorf("Cases = %+v, want %+v", b.Cases, want)
	}
}
