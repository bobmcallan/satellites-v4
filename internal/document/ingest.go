package document

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/ternarybob/arbor"
)

// ErrPathEscapes is returned by IngestFile when the supplied relative path
// resolves outside of docsDir (path-traversal attack protection).
var ErrPathEscapes = errors.New("document: path escapes docs directory")

// IngestFile reads docsDir/relPath and upserts a document keyed on
// (projectID, filename). Returns the resulting document and whether content
// changed (version bumped). Arbor logs one line per ingest carrying
// workspace_id + project_id + filename + version + bytes.
//
// relPath is rejected with ErrPathEscapes if it resolves outside docsDir
// (absolute paths or `..` traversal).
func IngestFile(ctx context.Context, store Store, logger arbor.ILogger, workspaceID, projectID, docsDir, relPath string, now time.Time) (UpsertResult, error) {
	abs, err := resolveInside(docsDir, relPath)
	if err != nil {
		return UpsertResult{}, err
	}
	body, err := os.ReadFile(abs)
	if err != nil {
		return UpsertResult{}, fmt.Errorf("document: read %q: %w", abs, err)
	}
	filename := filepath.Base(abs)
	docType := InferType(filename)
	res, err := store.Upsert(ctx, workspaceID, projectID, filename, docType, body, now)
	if err != nil {
		return UpsertResult{}, err
	}
	logger.Info().
		Str("event", "document-ingest").
		Str("workspace_id", workspaceID).
		Str("project_id", projectID).
		Str("filename", filename).
		Str("type", docType).
		Int("version", res.Document.Version).
		Int("bytes", len(body)).
		Bool("changed", res.Changed).
		Bool("created", res.Created).
		Msg("document ingest")
	return res, nil
}

func resolveInside(docsDir, relPath string) (string, error) {
	if filepath.IsAbs(relPath) {
		return "", ErrPathEscapes
	}
	rootAbs, err := filepath.Abs(docsDir)
	if err != nil {
		return "", fmt.Errorf("document: resolve docs dir %q: %w", docsDir, err)
	}
	joined := filepath.Clean(filepath.Join(rootAbs, relPath))
	root := rootAbs + string(filepath.Separator)
	if joined != rootAbs && !strings.HasPrefix(joined, root) {
		return "", ErrPathEscapes
	}
	return joined, nil
}
