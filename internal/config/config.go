// Package config loads the judge-server settings from environment variables.
package config

import (
	"fmt"
	"strconv"
	"strings"
)

// Config holds every setting of the judge-server. Byte fields are int64 and
// count bytes; MaxJobs and MaxQueue count jobs.
type Config struct {
	Listen         string
	TLSDir         string
	Token          string
	AllowedClients []string

	TestCaseDir string
	WorkDir     string

	MaxJobs  int
	MaxQueue int

	RunPoolBudget     int64
	CompileMaxMemory  int64
	GoCacheCap        int64
	OutputFileCap     int64
	OutputResponseCap int64
	CompileMessageCap int64
	InlineInputCap    int64

	LogFormat string
}

// Load reads the configuration through getenv (os.Getenv in production). An
// empty value means "use the default". A malformed number is an error that
// names the offending variable.
func Load(getenv func(string) string) (Config, error) {
	str := func(name, def string) string {
		if v := getenv(name); v != "" {
			return v
		}
		return def
	}

	cfg := Config{
		Listen:         str("LISTEN", ":8080"),
		TLSDir:         str("TLS_DIR", "/etc/oj/tls"),
		Token:          getenv("TOKEN"),
		AllowedClients: splitList(str("JUDGE_ALLOWED_CLIENTS", "judge-worker,run-worker")),
		TestCaseDir:    str("TEST_CASE_DIR", "/test_case"),
		WorkDir:        str("WORK_DIR", "/work"),
		LogFormat:      str("LOG_FORMAT", "json"),
	}

	var err error
	ints := []struct {
		name string
		def  int64
		dst  *int64
	}{
		{"RUN_POOL_BUDGET_BYTES", 1073741824, &cfg.RunPoolBudget},
		{"COMPILE_MAX_MEMORY_BYTES", 536870912, &cfg.CompileMaxMemory},
		{"GOCACHE_CAP_BYTES", 134217728, &cfg.GoCacheCap},
		{"OUTPUT_FILE_CAP_BYTES", 33554432, &cfg.OutputFileCap},
		{"OUTPUT_RESPONSE_CAP_BYTES", 65536, &cfg.OutputResponseCap},
		{"COMPILE_MESSAGE_CAP_BYTES", 65536, &cfg.CompileMessageCap},
		{"INLINE_INPUT_CAP_BYTES", 1048576, &cfg.InlineInputCap},
	}
	for _, f := range ints {
		if *f.dst, err = parseInt(getenv, f.name, f.def); err != nil {
			return Config{}, err
		}
	}

	maxJobs, err := parseInt(getenv, "MAX_JOBS", 2)
	if err != nil {
		return Config{}, err
	}
	maxQueue, err := parseInt(getenv, "MAX_QUEUE", 4)
	if err != nil {
		return Config{}, err
	}
	cfg.MaxJobs, cfg.MaxQueue = int(maxJobs), int(maxQueue)

	return cfg, nil
}

func parseInt(getenv func(string) string, name string, def int64) (int64, error) {
	v := getenv(name)
	if v == "" {
		return def, nil
	}
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("%s: %w", name, err)
	}
	return n, nil
}

// splitList splits a comma-separated list, trimming blanks and dropping
// empty entries.
func splitList(s string) []string {
	out := []string{}
	for _, part := range strings.Split(s, ",") {
		if p := strings.TrimSpace(part); p != "" {
			out = append(out, p)
		}
	}
	return out
}
