package retention

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/suankan/pocket-advisor/internal/discovery"
	"github.com/suankan/pocket-advisor/internal/domain"
	"github.com/suankan/pocket-advisor/internal/source"
)

var ErrStagingEmpty = errors.New("source directories contain no eligible files: refusing to delete every document")

// errReplayBudget stops one top-level file's expansion without ending the walk.
var errReplayBudget = errors.New("replay byte budget exhausted")

// staged is one traversal of the configured directories.
type staged struct {
	contents map[string]struct{}
	// ignored is the content of members the ignore rules exclude by name. A
	// recorded placement does not hold content seen only here: a container
	// still staged keeps its other members, not the ones the workspace now
	// leaves out. Content staged under another name stays in contents too.
	ignored map[string]struct{}
	files   int
	// Unexpandable counts containers whose members could not be re-derived —
	// a malformed multipart, an unknown charset, a corrupt archive, a replay
	// budget exhausted by one pathological file.
	//
	// It does not block a sweep. Retention reaches a container's members
	// through recorded containment (document_placements), and Plan refuses to
	// run at all until that containment is complete, so re-expansion here is a
	// redundant second source rather than the authority. A container that
	// cannot be parsed now could not be parsed at admission either, so its
	// members were never indexed as documents; one that could be is already
	// holding them through the placement graph. Real mail corpora carry a
	// steady fraction of these, and aborting on the first one only ever meant
	// refusing to ingest.
	unexpandable int
}

func stagedContents(roots []string, ignore func(string) bool) (staged, error) {
	return stagedContentsContext(context.Background(), roots, ignore, nil, nil)
}

// stagedContentsContext walks the configured directories, expanding
// containers into their members. ignoreMember, when set, excludes members by
// their own name: their content is recorded as ignored and not expanded.
func stagedContentsContext(ctx context.Context, roots []string, ignore, ignoreMember func(string) bool, log *slog.Logger) (staged, error) {
	r, err := source.New(roots, ignore, nil)
	if err != nil {
		return staged{}, err
	}
	defer r.Close()

	s := staged{contents: map[string]struct{}{}, ignored: map[string]struct{}{}}
	var expand func([]byte, string, int, string, *int64) error
	expand = func(data []byte, name string, depth int, origin string, budget *int64) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		*budget -= int64(len(data))
		if *budget < 0 {
			return errReplayBudget
		}
		s.contents[domain.SHA256Hex(data)] = struct{}{}
		if depth >= 8 {
			return nil
		}
		// Only what the formats unroll is expanded: a zip-based format they
		// refuse, such as an iWork document or a Wallet pass, is detected as a
		// zip, but its parts are not documents and nothing admits them, so
		// counting them as staged would keep them past their container.
		route := discovery.Classify(data, name)
		if route.Declined {
			return nil
		}
		switch route.MimeType {
		case "message/rfc822", "application/zip", "application/x-tar", "application/gzip", "application/x-gzip":
		default:
			return nil
		}
		children, err := source.Children(data, route.MimeType, name)
		if err != nil {
			s.unexpandable++
			// Named, because an operator chasing one of these has no other way
			// to find it: the count alone cannot be turned back into a file.
			if log != nil {
				log.Debug("container members could not be re-derived",
					"file", origin, "part", name, "depth", depth,
					"mime", route.MimeType, "error", err)
			}
			return nil
		}
		for _, c := range children {
			if ignoreMember != nil && ignoreMember(c.Filename) {
				s.ignored[domain.SHA256Hex(c.Data)] = struct{}{}
				continue
			}
			if err := expand(c.Data, c.Filename, depth+1, origin, budget); err != nil {
				return err
			}
		}
		return nil
	}

	err = r.Walk(ctx, func(f source.File, data []byte) error {
		s.files++
		budget := source.MaxReplayBytes
		err := expand(data, f.Filename, 0, f.DisplayPath, &budget)
		if errors.Is(err, errReplayBudget) {
			s.unexpandable++
			if log != nil {
				log.Debug("container expansion stopped at the replay byte limit",
					"file", f.DisplayPath, "limit", source.MaxReplayBytes)
			}
			return nil
		}
		return err
	})
	if err != nil {
		return s, fmt.Errorf("staged content traversal: %w", err)
	}
	return s, nil
}
