package judge

import (
	"reflect"
	"testing"
)

func TestExpandAll(t *testing.T) {
	v := Vars{
		SrcPath:         "/work/j1/main.c",
		ExePath:         "/work/j1/main",
		ExeDir:          "/work/j1",
		InFilePath:      "/test_case/p__abcdef12/1.in",
		UserOutFilePath: "/work/j1/1.out",
		SPJVersion:      "v7",
	}
	tmpl := "{src_path}|{exe_path}|{exe_dir}|{in_file_path}|{user_out_file_path}|{spj_version}"
	want := "/work/j1/main.c|/work/j1/main|/work/j1|/test_case/p__abcdef12/1.in|/work/j1/1.out|v7"
	if got := Expand(tmpl, v); got != want {
		t.Errorf("Expand = %q, want %q", got, want)
	}
}

func TestExpandRepeatedAndAdjacent(t *testing.T) {
	v := Vars{ExePath: "/e", ExeDir: "/d"}
	if got, want := Expand("{exe_dir}{exe_path} {exe_path}", v), "/d/e /e"; got != want {
		t.Errorf("Expand = %q, want %q", got, want)
	}
}

func TestExpandDoesNotExpandValues(t *testing.T) {
	// A value that looks like a placeholder is data, not a template.
	v := Vars{SrcPath: "{exe_path}", ExePath: "/e"}
	if got, want := Expand("{src_path} {exe_path}", v), "{exe_path} /e"; got != want {
		t.Errorf("Expand = %q, want %q", got, want)
	}
}

func TestExpandKeepsUnknown(t *testing.T) {
	v := Vars{SrcPath: "/s"}
	tmpl := "{foo} {src_path} {} {SRC_PATH} {src_path"
	want := "{foo} /s {} {SRC_PATH} {src_path"
	if got := Expand(tmpl, v); got != want {
		t.Errorf("Expand = %q, want %q", got, want)
	}
}

func TestSplitSingleSpaces(t *testing.T) {
	tests := []struct {
		in   string
		want []string
	}{
		{"a  b", []string{"a", "b"}},
		{"/usr/bin/gcc -o /w/main /w/main.c", []string{"/usr/bin/gcc", "-o", "/w/main", "/w/main.c"}},
		{"  lead and trail  ", []string{"lead", "and", "trail"}},
		{"one", []string{"one"}},
		// Only the space separates: a tab stays inside its field.
		{"a\tb c", []string{"a\tb", "c"}},
		// No spaces inside the -c argument, so it is one argv entry (a space in it would cut it in two).
		{
			"/usr/bin/python3 -c __import__('py_compile').compile('/w/s.py',cfile='/w/e.pyc',doraise=True)",
			[]string{"/usr/bin/python3", "-c", "__import__('py_compile').compile('/w/s.py',cfile='/w/e.pyc',doraise=True)"},
		},
	}
	for _, tt := range tests {
		got, err := Split(tt.in)
		if err != nil {
			t.Errorf("Split(%q): %v", tt.in, err)
			continue
		}
		if !reflect.DeepEqual(got, tt.want) {
			t.Errorf("Split(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

func TestSplitEmptyIsAnError(t *testing.T) {
	for _, in := range []string{"", " ", "   "} {
		if got, err := Split(in); err == nil {
			t.Errorf("Split(%q) = %q, want an error", in, got)
		}
	}
}

func TestValidateName(t *testing.T) {
	ok := []string{
		"Main",
		"main.c",
		"__pycache__/solution.pyc",
		"__pycache__/solution.cpython-36.pyc",
		"a/b/c",
		"..hidden", // "..hidden" is a name, not the ".." element
		"a..b",
	}
	for _, name := range ok {
		if err := ValidateName(name); err != nil {
			t.Errorf("ValidateName(%q) = %v, want nil", name, err)
		}
	}
	bad := []string{
		"",
		"/abs",
		"/",
		"../x",
		"..",
		"a/../../b",
		"a/../b", // any ".." element is refused, even one that stays inside
		"a/..",
		".",
	}
	for _, name := range bad {
		if err := ValidateName(name); err == nil {
			t.Errorf("ValidateName(%q) = nil, want an error", name)
		}
	}
}
