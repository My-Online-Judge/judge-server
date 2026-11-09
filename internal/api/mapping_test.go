package api

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/My-Online-Judge/judge-server/internal/bundle"
	"github.com/My-Online-Judge/judge-server/internal/judge"
)

func TestStatusFor(t *testing.T) {
	const secret = "/srv/secret/path"
	tests := []struct {
		name     string
		err      error
		bundleID string
		code     codes.Code
		message  string // exact, when not empty
	}{
		{"invalid bundle id", fmt.Errorf("%w: %q", bundle.ErrInvalidID, "x"), "x", codes.InvalidArgument, `invalid test case id: "x"`},
		{"bundle not found", fmt.Errorf("%w: %q: open %s: no such file", bundle.ErrNotFound, bundleID, secret), bundleID, codes.NotFound, "test-case bundle " + bundleID + " not on this sandbox"},
		{"bundle corrupt", fmt.Errorf("%w: %q: info: %s", bundle.ErrCorrupt, bundleID, secret), bundleID, codes.Internal, ""},
		{"engine bundle gone", fmt.Errorf("%w: case 1: lstat %s", judge.ErrBundle, secret), bundleID, codes.Internal, ""},
		{"invalid request", fmt.Errorf("%w: no test cases", judge.ErrInvalidRequest), "", codes.InvalidArgument, "invalid judge request: no test cases"},
		{"busy", ErrBusy, "", codes.ResourceExhausted, ""},
		{"wrapped busy", fmt.Errorf("admit: %w", ErrBusy), "", codes.ResourceExhausted, ""},
		{"cancelled", context.Canceled, "", codes.Canceled, ""},
		{"deadline", context.DeadlineExceeded, "", codes.DeadlineExceeded, ""},
		{"wrapped deadline", fmt.Errorf("wait for %s: %w", secret, context.DeadlineExceeded), "", codes.DeadlineExceeded, ""},
		{"anything else", fmt.Errorf("create %s: permission denied", secret), "", codes.Internal, "internal error"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			st, ok := status.FromError(statusFor(tc.err, tc.bundleID))
			if !ok || st.Code() != tc.code {
				t.Fatalf("status = %v, want code %v", st, tc.code)
			}
			if tc.message != "" && st.Message() != tc.message {
				t.Errorf("message = %q, want %q", st.Message(), tc.message)
			}
			if strings.Contains(st.Message(), secret) {
				t.Errorf("message %q leaks a server path", st.Message())
			}
		})
	}
}

func TestNewJobID(t *testing.T) {
	seen := map[string]bool{}
	for range 100 {
		id, err := newJobID()
		if err != nil {
			t.Fatalf("newJobID: %v", err)
		}
		if len(id) != 16 || strings.Trim(id, "0123456789abcdef") != "" {
			t.Fatalf("id %q is not 16 lowercase hex digits", id)
		}
		if seen[id] {
			t.Fatalf("id %q repeated", id)
		}
		seen[id] = true
	}
}

func TestToResponseEmptyOutcome(t *testing.T) {
	if _, err := toResponse(judge.Outcome{}); err == nil || errors.Is(err, judge.ErrInvalidRequest) {
		t.Fatalf("toResponse(empty) = %v, want an internal error", err)
	}
}
