package hitl

import (
	"errors"
	"fmt"
	"path"
	"strings"
)

// KindReview is a review request: the agent submits a file or a text/diff
// blob, and a human approves or rejects it with comments. the comments are
// the decision's reason, so they reach the agent either way.
const KindReview Kind = "review"

// ReviewRequests configures review requests. it has no fields yet;
// declaring the block is what allows the kind.
type ReviewRequests struct{}

// ReviewFormat says how a review's content should be shown.
type ReviewFormat string

const (
	ReviewText ReviewFormat = "text"
	ReviewDiff ReviewFormat = "diff"
)

// MaxReviewBytes bounds a review's content, inline or read from a file.
const MaxReviewBytes = 1 << 20

// ReviewPayload is what is up for review. Path is set when the agent named
// a guest file; Content is then that file as fuse read it at request time,
// so the human reviews exactly what the agent submitted.
type ReviewPayload struct {
	Path    string       `json:"path,omitempty"`
	Content string       `json:"content"`
	Format  ReviewFormat `json:"format"`
	Message string       `json:"message,omitempty"`
	// Truncated is set when a file was longer than MaxReviewBytes.
	Truncated bool `json:"truncated,omitempty"`
}

// ValidateReviewRequest checks what the agent sent: exactly one of a path
// and inline content, a known format, and a bounded message.
func ValidateReviewRequest(p ReviewPayload) (ReviewPayload, error) {
	switch {
	case p.Path != "" && p.Content != "":
		return p, errors.New("send either path or content, not both")
	case p.Path == "" && strings.TrimSpace(p.Content) == "":
		return p, errors.New("one of path or content is required")
	case p.Path != "" && (!path.IsAbs(p.Path) || path.Clean(p.Path) != p.Path):
		return p, fmt.Errorf("path must be a clean absolute guest path, got %q", p.Path)
	case len(p.Content) > MaxReviewBytes:
		return p, fmt.Errorf("content must be at most %d bytes", MaxReviewBytes)
	case len(p.Message) > maxQuestionLen:
		return p, fmt.Errorf("message must be at most %d bytes", maxQuestionLen)
	}
	switch p.Format {
	case "":
		p.Format = ReviewText
		if looksLikeDiff(p.Content) || strings.HasSuffix(p.Path, ".diff") || strings.HasSuffix(p.Path, ".patch") {
			p.Format = ReviewDiff
		}
	case ReviewText, ReviewDiff:
	default:
		return p, fmt.Errorf("format must be text or diff, got %q", p.Format)
	}
	return p, nil
}

// looksLikeDiff spots a unified diff by its headers.
func looksLikeDiff(s string) bool {
	return strings.HasPrefix(s, "diff --git ") || strings.HasPrefix(s, "--- ") && strings.Contains(s, "\n+++ ")
}
