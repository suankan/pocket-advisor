package retention

import (
	"context"
	"errors"
	"fmt"
	"github.com/suankan/pocket-advisor/internal/discovery"
	"github.com/suankan/pocket-advisor/internal/domain"
	"github.com/suankan/pocket-advisor/internal/source"
)

var ErrStagingEmpty = errors.New("source directories contain no eligible files: refusing to delete every document")

func stagedContents(roots []string, ignore func(string) bool) (map[string]struct{}, int, error) {
	return stagedContentsContext(context.Background(), roots, ignore)
}
func stagedContentsContext(ctx context.Context, roots []string, ignore func(string) bool) (map[string]struct{}, int, error) {
	r, err := source.New(roots, ignore, nil)
	if err != nil {
		return nil, 0, err
	}
	defer r.Close()
	contents := map[string]struct{}{}
	files := 0
	var expand func([]byte, string, int, *int64) error
	expand = func(data []byte, name string, depth int, budget *int64) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		*budget -= int64(len(data))
		if *budget < 0 {
			return fmt.Errorf("retention replay byte limit exceeded")
		}
		contents[domain.SHA256Hex(data)] = struct{}{}
		if depth >= 8 {
			return nil
		}
		route := discovery.Classify(data, name)
		switch route.MimeType {
		case "message/rfc822", "application/zip", "application/x-tar", "application/gzip", "application/x-gzip":
		default:
			return nil
		}
		children, err := source.Children(data, route.MimeType, name)
		if err != nil {
			return fmt.Errorf("retention container traversal incomplete: %w", err)
		}
		for _, c := range children {
			if err := expand(c.Data, c.Filename, depth+1, budget); err != nil {
				return err
			}
		}
		return nil
	}
	err = r.Walk(ctx, func(f source.File, data []byte) error {
		files++
		budget := source.MaxReplayBytes
		return expand(data, f.Filename, 0, &budget)
	})
	return contents, files, err
}
