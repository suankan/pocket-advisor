package worker

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"time"

	"github.com/suankan/pocket-advisor/internal/domain"
	"github.com/suankan/pocket-advisor/internal/engine/email"
	"github.com/suankan/pocket-advisor/internal/source"
)

// Containment backfill re-derives, from verified originals, the containments that ingest
// discarded before document_placements existed.
//
// The gap it fills is narrow and total: a container's children were recorded
// only when the child's content was new to the workspace, so the second email
// to carry an attachment left no trace of carrying it. Seeding placements from
// documents.parent_doc_id reproduces exactly that under-count, and sweeping
// against an under-count deletes attachments that something still holds. The
// only authority that can say what a message contained is the message, so this
// reads every container's bytes back and asks again.
//
// It writes nothing but placements: no status, no text, no chunk, no metadata.
// A container whose original source is gone is reported, never guessed at.

// PlacementContainerSelector walks the workspace's container documents.
type PlacementContainerSelector interface {
	ContainerDocuments(ctx context.Context, after string, limit int) ([]domain.Document, error)
}

// PlacementWriter records one containment and resolves content to documents.
type PlacementWriter interface {
	RecordPlacement(ctx context.Context, containerID, containedID string) error
	DocIDBySHA(ctx context.Context, sha string) (string, error)
	HasPlacements(ctx context.Context, containerID string) (bool, error)
	MarkPlacementsBackfilled(ctx context.Context, at time.Time) error
}

// PlacementObjectReader reads verified original files, the only authority for what a container
// held.
type PlacementObjectReader = source.Reader

// PlacementBackfiller rebuilds containment for one workspace.
type PlacementBackfiller struct {
	Docs    PlacementContainerSelector
	Write   PlacementWriter
	Sources PlacementObjectReader
	Log     *slog.Logger
}

func (b *PlacementBackfiller) log() *slog.Logger {
	if b.Log != nil {
		return b.Log
	}
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// PlacementBackfillSummary reports only counts and closed reason codes. A
// container's filename, subject and path are private to the workspace and have
// no business in a maintenance log.
type PlacementBackfillSummary struct {
	Containers int `json:"containers"`
	Placements int `json:"placements"`
	// Unreadable is a container whose original bytes could not be read at all.
	Unreadable int `json:"unreadable"`
	// Unparsable is bytes that were read and could not be unrolled.
	Unparsable int `json:"unparsable"`
	// Unknown is a child whose content has no document in this workspace,
	// which means an extraction that never completed rather than a placement
	// to record.
	Unknown int `json:"unknown_children"`
	// UnparsableWithChildren counts containers that cannot be unrolled now yet
	// already have recorded children. That combination is the only reason an
	// unparsable container is dangerous: it proves the container did produce
	// children once, so the ones this pass cannot see may be missing from the
	// graph. A container that fails to parse and has no children never
	// contributed any — the same parser rejected it at ingest — and its
	// contribution is genuinely nothing rather than something unseen.
	UnparsableWithChildren int  `json:"unparsable_with_children"`
	Complete               bool `json:"complete"`
}

// Run walks every container and records what it contains.
//
// Completion is all-or-nothing on purpose. A partial walk leaves exactly the
// under-counted placement table the sweep must not run against, so the marker
// is written only when every container has been read; anything unreadable
// leaves it unset and pruning stays refused.
func (b *PlacementBackfiller) Run(ctx context.Context, batch int) (PlacementBackfillSummary, error) {
	if batch < 1 {
		batch = 200
	}
	var s PlacementBackfillSummary
	after := ""

	for {
		if err := ctx.Err(); err != nil {
			return s, err
		}
		docs, err := b.Docs.ContainerDocuments(ctx, after, batch)
		if err != nil {
			return s, err
		}
		if len(docs) == 0 {
			break
		}
		for _, doc := range docs {
			after = doc.DocID
			s.Containers++
			n, err := b.one(ctx, doc)
			s.Placements += n
			switch {
			case err == nil:
			case errors.Is(err, errUnreadable):
				s.Unreadable++
			case errors.Is(err, errUnparsable):
				s.Unparsable++
				had, cerr := b.Write.HasPlacements(ctx, doc.DocID)
				if cerr != nil {
					return s, cerr
				}
				if had {
					s.UnparsableWithChildren++
				}
				// doc_id only: enough to go and look, and it carries none of
				// the message. The aggregate above is what a summary reports.
				b.log().Debug("container could not be unrolled",
					"doc_id", doc.DocID, "doc_type", doc.DocType, "mime_type", doc.MimeType)
			default:
				return s, err
			}
		}
		b.log().Info("containment backfill progress",
			"containers", s.Containers, "placements", s.Placements)
	}

	// Unreadable blocks: the container was ingested, so children may exist that
	// this pass cannot enumerate. Unparsable blocks only when the container
	// demonstrably has children, since otherwise the same parser rejected it at
	// ingest and it never produced any — waiting for a container that can never
	// be unrolled would disable pruning permanently, which is its own failure.
	s.Complete = s.Unreadable == 0 && s.UnparsableWithChildren == 0
	if s.Complete {
		if err := b.Write.MarkPlacementsBackfilled(ctx, time.Now().UTC()); err != nil {
			return s, err
		}
	}
	b.log().Info("containment backfill finished",
		"containers", s.Containers, "placements", s.Placements,
		"unreadable", s.Unreadable, "unparsable", s.Unparsable,
		"unparsable_with_children", s.UnparsableWithChildren,
		"unknown_children", s.Unknown, "complete", s.Complete)
	return s, nil
}

var (
	errUnreadable = errors.New("original source unreadable")
	errUnparsable = errors.New("container could not be unrolled")
)

// one re-derives a single container's children through the same unrolling the
// email worker uses, so the two cannot disagree about what a container holds.
func (b *PlacementBackfiller) one(ctx context.Context, doc domain.Document) (int, error) {
	data, err := b.Sources.Read(ctx, source.Ref(doc.DocID), doc.RawSHA256)
	if err != nil {
		return 0, fmt.Errorf("%w: %s", errUnreadable, doc.DocID)
	}

	var children []email.Child
	if isArchive(doc.MimeType) {
		children, err = email.UnrollArchive(data, doc.SourceName)
	} else {
		var parsed *email.Parsed
		parsed, err = email.ParseEmail(data)
		if parsed != nil {
			children = parsed.Children
		}
	}
	if err != nil {
		return 0, fmt.Errorf("%w: %s", errUnparsable, doc.DocID)
	}

	recorded := 0
	for _, c := range children {
		if len(c.Data) == 0 {
			continue
		}
		childID, err := b.Write.DocIDBySHA(ctx, domain.SHA256Hex(c.Data))
		if err != nil {
			return recorded, err
		}
		if childID == "" || childID == doc.DocID {
			// No document for this content: its extraction never completed, so
			// there is nothing to hold in place. Recording is not possible and
			// inventing a row would be worse than the gap.
			continue
		}
		if err := b.Write.RecordPlacement(ctx, doc.DocID, childID); err != nil {
			return recorded, err
		}
		recorded++
	}
	return recorded, nil
}
