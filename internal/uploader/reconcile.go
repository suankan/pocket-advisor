package uploader

import (
	"fmt"
	"io/fs"
	"path/filepath"
)

// What a workspace currently stages, which is the input every deletion
// decision is made against. The sweep that consumes it lives in prune.go; this
// file answers only "what is on disk right now", and answers it by content.

// ErrStagingEmpty refuses any sweep when the staging directories yielded no
// files at all.
//
// An absent, unmounted, or half-synced directory is indistinguishable from a
// deliberately emptied one, and the difference is the entire corpus. Refusing
// is the only safe reading: a genuinely emptied workspace is served by
// --delete-data, which says what it does.
var ErrStagingEmpty = fmt.Errorf("staging directory contains no files: refusing to treat this as a request to delete every document")

// stagedContents hashes every file under every root and returns the set of
// content hashes present anywhere among them.
//
// Identity is content, never path. Roughly half of a real staging directory
// can be the same bytes under two names, and documents are deduplicated on
// raw_sha256, so a document records only the first path its content was seen
// at. Judging by path would call a document deleted while its bytes are still
// on disk under the other name — and the next ingest would re-upload them
// under a fresh doc_id, silently invalidating anything that referenced the
// old one. Hashing also makes the comparison immune to filename
// normalization, which differs between what a filesystem stores and what was
// recorded. A workspace's several roots are one staging feed for this
// purpose: content moved from one configured directory to another is still
// staged.
//
// The workspace's ignore patterns apply here exactly as they do to an upload,
// and deliberately so: "staged" has to mean one thing. If a pattern excluded a
// file from the corpus, its content is not staged, and a document that came
// from it before the pattern existed is a document whose source the workspace
// has since disowned — which is precisely what this command reports.
func stagedContents(roots []string, ignore func(rel string) bool) (map[string]struct{}, int, error) {
	contents := make(map[string]struct{})
	files := 0
	for _, root := range roots {
		err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() || !d.Type().IsRegular() {
				return nil
			}
			if ignoredBy(ignore, root, path) {
				return nil
			}
			sum, _, err := hashFile(path)
			if err != nil {
				return err
			}
			files++
			contents[sum] = struct{}{}
			return nil
		})
		if err != nil {
			return nil, 0, fmt.Errorf("walk staging directory %s: %w", root, err)
		}
	}
	return contents, files, nil
}
