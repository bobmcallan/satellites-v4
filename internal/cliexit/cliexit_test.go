package cliexit_test

import (
	"errors"
	"fmt"
	"testing"

	"github.com/bobmcallan/satellites/internal/cliexit"
)

func TestResolve_NilReturnsOK(t *testing.T) {
	if got := cliexit.Resolve(nil); got != cliexit.OK {
		t.Fatalf("Resolve(nil) = %d, want %d", got, cliexit.OK)
	}
}

func TestResolve_PlainErrorReturnsServer(t *testing.T) {
	if got := cliexit.Resolve(errors.New("boom")); got != cliexit.Server {
		t.Fatalf("Resolve(plain) = %d, want %d", got, cliexit.Server)
	}
}

func TestResolve_TypedReturnsCode(t *testing.T) {
	cases := []cliexit.Code{cliexit.Usage, cliexit.NotFound, cliexit.Auth, cliexit.Server, cliexit.RateLimit}
	for _, code := range cases {
		err := cliexit.Newf(code, "boom %d", int(code))
		if got := cliexit.Resolve(err); got != code {
			t.Fatalf("Resolve(Newf(%d)) = %d, want %d", code, got, code)
		}
	}
}

func TestResolve_WrappedTypedReturnsCode(t *testing.T) {
	inner := cliexit.Newf(cliexit.NotFound, "missing id")
	wrapped := fmt.Errorf("context: %w", inner)
	if got := cliexit.Resolve(wrapped); got != cliexit.NotFound {
		t.Fatalf("Resolve(wrapped) = %d, want %d", got, cliexit.NotFound)
	}
}

func TestNotImplemented_IsServer(t *testing.T) {
	err := cliexit.NotImplemented("story get")
	if got := cliexit.Resolve(err); got != cliexit.Server {
		t.Fatalf("NotImplemented exit = %d, want %d", got, cliexit.Server)
	}
	if got := err.Error(); got == "" {
		t.Fatalf("NotImplemented error empty")
	}
}

func TestWrap_NilReturnsNil(t *testing.T) {
	if cliexit.Wrap(cliexit.Usage, nil) != nil {
		t.Fatal("Wrap(_, nil) should return nil")
	}
}
