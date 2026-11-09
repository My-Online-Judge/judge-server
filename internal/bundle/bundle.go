// Package bundle reads the test-case bundles judge-worker keeps under the
// test-case root, and builds the equivalent in-memory bundle for inline cases.
//
// A bundle lives in <root>/<slug>__<hash>/ and holds an "info" JSON file plus
// one input file per case. The worker names the directory with the same
// "<slug>__<hash>" id (it splits on the last "__"), so ValidateID accepts
// exactly that shape and nothing that could reach outside the root.
package bundle

import (
	"cmp"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/My-Online-Judge/judge-server/internal/compare"
)

// The errors Load returns; test them with errors.Is.
var (
	// ErrInvalidID: the id is not "<slug>__<hash>", or it resolves outside the root.
	ErrInvalidID = errors.New("invalid test case id")
	// ErrNotFound: the bundle directory or its info file does not exist.
	ErrNotFound = errors.New("test case bundle not found")
	// ErrCorrupt: the bundle exists but its info file or inputs are unusable.
	ErrCorrupt = errors.New("test case bundle is corrupt")
)

var (
	idPattern  = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*__[0-9a-f]{8,128}$`)
	md5Pattern = regexp.MustCompile(`^[0-9a-f]{32}$`)
)

// Case is one test case. A bundle case has InputPath (an existing regular file)
// and nil InputBytes; an inline case has InputBytes and an empty InputPath, and
// the job writes the input where the program reads it.
type Case struct {
	Name              string
	InputPath         string
	StrippedOutputMD5 string
	InputBytes        []byte
}

// Bundle is the ordered set of cases of one problem. Cases are in numeric order
// when every name is an integer, else in lexicographic order. Dir is empty for
// an inline bundle.
type Bundle struct {
	Dir        string
	SPJ        bool
	SPJVersion string
	Cases      []Case
}

// InlineCase is one case sent with the request: its input and expected output.
type InlineCase struct {
	Input, Output []byte
}

// ValidateID accepts "<slug>__<hash>": a slug starting with a letter or digit,
// then "__", then 8 to 128 lowercase hex digits. The result never contains a
// path separator or a "..".
func ValidateID(id string) error {
	if !idPattern.MatchString(id) {
		return fmt.Errorf("%w: %q", ErrInvalidID, id)
	}
	return nil
}

// infoFile is the part of the bundle's info file judge-server reads; the
// remaining keys (sizes, output_md5, output_name) are ignored.
type infoFile struct {
	TestCaseNumber int                 `json:"test_case_number"`
	SPJ            bool                `json:"spj"`
	SPJVersion     string              `json:"spj_version"`
	TestCases      map[string]infoCase `json:"test_cases"`
}

type infoCase struct {
	InputName         string `json:"input_name"`
	StrippedOutputMD5 string `json:"stripped_output_md5"`
}

// Load reads the bundle <root>/<id>. The root and the bundle directory are
// resolved through symlinks, and a bundle that resolves outside the root is
// refused with ErrInvalidID. An info file with no cases is ErrCorrupt: judging
// against nothing would accept every submission.
func Load(root, id string) (*Bundle, error) {
	if err := ValidateID(id); err != nil {
		return nil, err
	}
	realRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		return nil, wrapMissing(err, "resolve root", id)
	}
	dir, err := filepath.EvalSymlinks(filepath.Join(realRoot, id))
	if err != nil {
		return nil, wrapMissing(err, "resolve bundle", id)
	}
	if !within(realRoot, dir) {
		return nil, fmt.Errorf("%w: %q resolves outside the test case root", ErrInvalidID, id)
	}
	if fi, err := os.Stat(dir); err != nil {
		return nil, wrapMissing(err, "stat bundle", id)
	} else if !fi.IsDir() {
		return nil, fmt.Errorf("%w: %q is not a directory", ErrNotFound, id)
	}

	raw, err := os.ReadFile(filepath.Join(dir, "info"))
	if err != nil {
		return nil, wrapMissing(err, "read info", id)
	}
	var info infoFile
	if err := json.Unmarshal(raw, &info); err != nil {
		return nil, fmt.Errorf("%w: %q: info: %v", ErrCorrupt, id, err)
	}
	if len(info.TestCases) == 0 {
		return nil, fmt.Errorf("%w: %q: info lists no test cases", ErrCorrupt, id)
	}
	if info.TestCaseNumber != len(info.TestCases) {
		return nil, fmt.Errorf("%w: %q: test_case_number %d but %d test_cases", ErrCorrupt, id, info.TestCaseNumber, len(info.TestCases))
	}

	cases := make([]Case, 0, len(info.TestCases))
	for name, tc := range info.TestCases {
		if !plainName(name) || !plainName(tc.InputName) {
			return nil, fmt.Errorf("%w: %q: case %q has an unsafe name or input_name %q", ErrCorrupt, id, name, tc.InputName)
		}
		if !md5Pattern.MatchString(tc.StrippedOutputMD5) {
			return nil, fmt.Errorf("%w: %q: case %q has a bad stripped_output_md5", ErrCorrupt, id, name)
		}
		inputPath := filepath.Join(dir, tc.InputName)
		fi, err := os.Lstat(inputPath)
		if errors.Is(err, fs.ErrNotExist) {
			return nil, fmt.Errorf("%w: %q: input %s is missing", ErrCorrupt, id, tc.InputName)
		}
		if err != nil {
			return nil, fmt.Errorf("stat input %s of %q: %w", tc.InputName, id, err)
		}
		if !fi.Mode().IsRegular() {
			return nil, fmt.Errorf("%w: %q: input %s is not a regular file", ErrCorrupt, id, tc.InputName)
		}
		cases = append(cases, Case{Name: name, InputPath: inputPath, StrippedOutputMD5: tc.StrippedOutputMD5})
	}
	sortCases(cases)
	return &Bundle{Dir: dir, SPJ: info.SPJ, SPJVersion: info.SPJVersion, Cases: cases}, nil
}

// Inline builds the bundle of cases sent with a request: names "1".."n", no
// directory, and each case keeps its input bytes. The slices are retained, not
// copied.
func Inline(cases []InlineCase) *Bundle {
	out := make([]Case, len(cases))
	for i, c := range cases {
		out[i] = Case{
			Name:              strconv.Itoa(i + 1),
			StrippedOutputMD5: compare.MD5Hex(compare.Normalize(c.Output)),
			InputBytes:        c.Input,
		}
	}
	return &Bundle{Cases: out}
}

// wrapMissing turns a missing file or directory into ErrNotFound and wraps any
// other error with its context.
func wrapMissing(err error, what, id string) error {
	if errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("%w: %q: %v", ErrNotFound, id, err)
	}
	return fmt.Errorf("%s of %q: %w", what, id, err)
}

// within reports whether child is strictly inside root (both already resolved).
func within(root, child string) bool {
	rel, err := filepath.Rel(root, child)
	if err != nil {
		return false
	}
	return rel != "." && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// plainName reports whether s is usable as a single file name inside the bundle
// directory: not empty, not "." or "..", and no "/" or NUL.
func plainName(s string) bool {
	return s != "" && s != "." && s != ".." && !strings.ContainsAny(s, "/\x00")
}

// sortCases orders cases numerically when every name is an unsigned integer
// (ties, such as "01" and "1", fall back to the name), else lexicographically.
func sortCases(cases []Case) {
	nums := make(map[string]uint64, len(cases))
	numeric := true
	for _, c := range cases {
		n, err := strconv.ParseUint(c.Name, 10, 64)
		if err != nil {
			numeric = false
			break
		}
		nums[c.Name] = n
	}
	slices.SortFunc(cases, func(a, b Case) int {
		if numeric {
			if c := cmp.Compare(nums[a.Name], nums[b.Name]); c != 0 {
				return c
			}
		}
		return strings.Compare(a.Name, b.Name)
	})
}
