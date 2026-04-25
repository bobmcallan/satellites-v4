package codeindex

import (
	"context"
	"errors"
	"testing"
)

func TestUnavailableError_IsAndUnwrap(t *testing.T) {
	t.Parallel()
	root := errors.New("connection refused")
	ue := &UnavailableError{Op: "search_symbols", Err: root}

	if !errors.Is(ue, ErrUnavailable) {
		t.Fatalf("errors.Is(ue, ErrUnavailable) = false, want true")
	}
	if !errors.Is(ue, root) {
		t.Fatalf("errors.Is(ue, root) = false, want true (Unwrap broken)")
	}
	if got := ue.Error(); got == "" {
		t.Fatalf("Error() returned empty string")
	}
}

func TestStub_AllMethodsReturnUnavailable(t *testing.T) {
	t.Parallel()
	s := NewStub()
	ctx := context.Background()

	if _, err := s.IndexRepo(ctx, "git@host:r.git", "main"); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("IndexRepo: err = %v, want ErrUnavailable", err)
	}
	if _, err := s.ListRepos(ctx); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("ListRepos: err = %v, want ErrUnavailable", err)
	}
	if _, err := s.SearchSymbols(ctx, "k", "q", "", ""); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("SearchSymbols: err = %v", err)
	}
	if _, err := s.SearchText(ctx, "k", "q", ""); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("SearchText: err = %v", err)
	}
	if _, err := s.GetSymbolSource(ctx, "k", "sym"); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("GetSymbolSource: err = %v", err)
	}
	if _, err := s.GetFileContent(ctx, "k", "p"); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("GetFileContent: err = %v", err)
	}
	if _, err := s.GetFileOutline(ctx, "k", "p"); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("GetFileOutline: err = %v", err)
	}
}
