// Package judge turns a judge request into runs: it expands the qduoj command
// templates the worker sends and derives a case's result code from the raw
// sandbox outcome.
package judge

import (
	"errors"
	"fmt"
	"path/filepath"
	"strings"
)

// Vars are the values the command placeholders stand for. Empty fields expand
// to the empty string.
type Vars struct {
	SrcPath, ExePath, ExeDir, InFilePath, UserOutFilePath, SPJVersion string
}

// Expand replaces {src_path}, {exe_path}, {exe_dir}, {in_file_path},
// {user_out_file_path} and {spj_version} in template. Any other {x} is left as
// it is, and the substituted values are not scanned again.
func Expand(template string, v Vars) string {
	return strings.NewReplacer(
		"{src_path}", v.SrcPath,
		"{exe_path}", v.ExePath,
		"{exe_dir}", v.ExeDir,
		"{in_file_path}", v.InFilePath,
		"{user_out_file_path}", v.UserOutFilePath,
		"{spj_version}", v.SPJVersion,
	).Replace(template)
}

// Split cuts a command into argv at single spaces and drops empty fields, the
// way qduoj does; there is no quoting. It is an error when nothing is left.
func Split(cmd string) ([]string, error) {
	var argv []string
	for _, f := range strings.Split(cmd, " ") {
		if f != "" {
			argv = append(argv, f)
		}
	}
	if len(argv) == 0 {
		return nil, errors.New("empty command")
	}
	return argv, nil
}

// ValidateName accepts a file name that stays inside the job directory: not
// empty, relative, no ".." element, and not the directory itself. A relative
// sub-path such as "__pycache__/solution.pyc" is fine.
func ValidateName(name string) error {
	if name == "" {
		return errors.New("empty file name")
	}
	if filepath.IsAbs(name) {
		return fmt.Errorf("file name %q is absolute", name)
	}
	for _, el := range strings.Split(name, "/") {
		if el == ".." {
			return fmt.Errorf("file name %q contains a \"..\" element", name)
		}
	}
	if filepath.Clean(name) == "." {
		return fmt.Errorf("file name %q is the job directory itself", name)
	}
	return nil
}
