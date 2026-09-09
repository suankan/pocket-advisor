// Package source opens original workspace files and replays container members.
// It never writes source bytes or accepts caller-selected filesystem scope.
package source

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/google/uuid"
	"github.com/suankan/pocket-advisor/internal/domain"
	"github.com/suankan/pocket-advisor/internal/engine/email"
)

const MaxBytes int64 = 256 << 20
const MaxReplayBytes int64 = 512 << 20

var ErrUnavailable = errors.New("source unavailable")
var ErrChanged = errors.New("source content changed")
var ErrUnsafe = errors.New("source path is not confined to a configured root")

type File struct {
	RootKey      string
	RelativePath string
	DisplayPath  string
	Filename     string
	SHA256       string
	Size         int64
	ModifiedAt   time.Time
}
type Member struct {
	ParentID string
	Selector string
	SHA256   string
}
type Record struct {
	DocID    string
	SHA256   string
	Filename string
	MimeType string
	Files    []File
	Members  []Member
}
type Store interface {
	SourceRecord(context.Context, string) (Record, error)
}
type Reader interface {
	Read(context.Context, string, string) ([]byte, error)
}
type root struct {
	key, name string
	dir       *os.Root
}
type Resolver struct {
	roots  []root
	ignore func(string) bool
	Store  Store
}

func New(paths []string, ignore func(string) bool, store Store) (*Resolver, error) {
	r := &Resolver{ignore: ignore, Store: store}
	for _, p := range paths {
		abs, err := filepath.Abs(p)
		if err != nil {
			r.Close()
			return nil, err
		}
		// Opening roots is deferred: cached database evidence remains usable when
		// an original volume is offline. Each read opens the configured root anew.
		r.roots = append(r.roots, root{key: domain.SHA256Hex([]byte(filepath.Clean(abs))), name: abs})
	}
	sort.Slice(r.roots, func(i, j int) bool { return r.roots[i].key < r.roots[j].key })
	return r, nil
}
func (r *Resolver) Close() {
	for _, root := range r.roots {
		if root.dir != nil {
			_ = root.dir.Close()
		}
	}
}
func (r *Resolver) RootKeys() []string {
	keys := make([]string, 0, len(r.roots))
	for _, root := range r.roots {
		keys = append(keys, root.key)
	}
	return keys
}
func Ref(docID string) string { return "source:" + docID }
func ParseRef(ref string) (string, error) {
	id := strings.TrimPrefix(ref, "source:")
	if ref == id {
		return "", ErrUnsafe
	}
	u, err := uuid.Parse(id)
	if err != nil || u.String() != id {
		return "", ErrUnsafe
	}
	return id, nil
}
func (r *Resolver) rootFor(key string) (root, error) {
	for _, v := range r.roots {
		if v.key == key {
			return v, nil
		}
	}
	return root{}, ErrUnsafe
}
func validPath(p string) bool { return fs.ValidPath(p) && p != "." && !strings.Contains(p, "\\") }
func (r *Resolver) readFile(ctx context.Context, f File) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if !validPath(f.RelativePath) || (r.ignore != nil && r.ignore(f.RelativePath)) {
		return nil, ErrUnsafe
	}
	root, err := r.rootFor(f.RootKey)
	if err != nil {
		return nil, err
	}
	dir, err := os.OpenRoot(root.name)
	if err != nil {
		return nil, ErrUnavailable
	}
	defer dir.Close()
	// Reject observed symlinks. os.Root also confines path replacement races.
	parts := strings.Split(f.RelativePath, "/")
	for i := range parts {
		st, e := dir.Lstat(strings.Join(parts[:i+1], "/"))
		if e != nil {
			return nil, ErrUnavailable
		}
		if st.Mode()&os.ModeSymlink != 0 {
			return nil, ErrUnsafe
		}
	}
	file, err := dir.OpenFile(f.RelativePath, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, ErrUnavailable
	}
	defer file.Close()
	before, err := file.Stat()
	if err != nil || !before.Mode().IsRegular() {
		return nil, ErrUnsafe
	}
	if before.Size() > MaxBytes {
		return nil, fmt.Errorf("source exceeds %d byte limit", MaxBytes)
	}
	data, err := io.ReadAll(io.LimitReader(&contextReader{ctx: ctx, r: file}, MaxBytes+1))
	if err != nil {
		return nil, err
	}
	after, err := file.Stat()
	if err != nil {
		return nil, ErrUnavailable
	}
	if int64(len(data)) > MaxBytes || before.Size() != after.Size() || !before.ModTime().Equal(after.ModTime()) || int64(len(data)) != after.Size() {
		return nil, ErrChanged
	}
	if f.SHA256 != "" && domain.SHA256Hex(data) != f.SHA256 {
		return nil, ErrChanged
	}
	return data, nil
}

type contextReader struct {
	ctx context.Context
	r   io.Reader
}

func (r *contextReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.r.Read(p)
}
func (r *Resolver) ReadFile(ctx context.Context, f File) ([]byte, error) { return r.readFile(ctx, f) }

// Walk reads and hashes every eligible regular file. It calls visit serially,
// bounding live raw buffers to one source and propagating incomplete scans.
func (r *Resolver) Walk(ctx context.Context, visit func(File, []byte) error) error {
	for _, root := range r.roots {
		dir, err := os.OpenRoot(root.name)
		if err != nil {
			return ErrUnavailable
		}
		err = fs.WalkDir(dir.FS(), ".", func(rel string, d fs.DirEntry, walkErr error) error {
			if walkErr != nil {
				return ErrUnavailable
			}
			if err := ctx.Err(); err != nil {
				return err
			}
			if d.IsDir() || !d.Type().IsRegular() {
				return nil
			}
			if d.Name() == ".DS_Store" || d.Name() == "Thumbs.db" || strings.HasPrefix(d.Name(), "._") {
				return nil
			}
			if r.ignore != nil && r.ignore(rel) {
				return nil
			}
			st, err := d.Info()
			if err != nil {
				return ErrUnavailable
			}
			f := File{RootKey: root.key, RelativePath: rel, DisplayPath: rel, Filename: path.Base(rel), Size: st.Size(), ModifiedAt: st.ModTime()}
			if len(r.roots) > 1 {
				f.DisplayPath = filepath.Base(root.name) + "/" + rel
			}
			data, err := r.readFile(ctx, f)
			if err != nil {
				return err
			}
			f.SHA256 = domain.SHA256Hex(data)
			f.Size = int64(len(data))
			return visit(f, data)
		})
		_ = dir.Close()
		if err != nil {
			return err
		}
	}
	return nil
}
func (r *Resolver) Read(ctx context.Context, ref, expected string) ([]byte, error) {
	id, err := ParseRef(ref)
	if err != nil {
		return nil, err
	}
	budget := MaxReplayBytes
	return r.read(ctx, id, expected, map[string]bool{}, 0, &budget)
}
func (r *Resolver) read(ctx context.Context, id, expected string, seen map[string]bool, depth int, budget *int64) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if depth > email.MaxDepth || seen[id] {
		return nil, ErrUnavailable
	}
	seen[id] = true
	defer delete(seen, id)
	rec, err := r.Store.SourceRecord(ctx, id)
	if err != nil {
		return nil, err
	}
	if expected != "" && rec.SHA256 != expected {
		return nil, ErrChanged
	}
	resultErr := ErrUnavailable
	for _, f := range rec.Files {
		f.SHA256 = rec.SHA256
		data, e := r.readFile(ctx, f)
		if e == nil {
			*budget -= int64(len(data))
			if *budget < 0 {
				return nil, fmt.Errorf("source replay byte limit exceeded")
			}
			return data, nil
		}
		if errors.Is(e, ErrChanged) {
			resultErr = ErrChanged
		}
	}
	for _, m := range rec.Members {
		parent, e := r.Store.SourceRecord(ctx, m.ParentID)
		if e != nil {
			continue
		}
		data, e := r.read(ctx, m.ParentID, parent.SHA256, seen, depth+1, budget)
		if e != nil {
			if errors.Is(e, ErrChanged) {
				resultErr = ErrChanged
			}
			continue
		}
		children, e := Children(data, parent.MimeType, parent.Filename)
		if e != nil {
			continue
		}
		for _, c := range children {
			*budget -= int64(len(c.Data))
			if *budget < 0 {
				return nil, fmt.Errorf("source replay byte limit exceeded")
			}
		}
		for _, c := range children {
			if c.Selector == m.Selector && domain.SHA256Hex(c.Data) == rec.SHA256 {
				return c.Data, nil
			}
		}
	}
	return nil, resultErr
}
func Children(data []byte, mime, name string) ([]email.Child, error) {
	if mime == "message/rfc822" {
		p, err := email.ParseEmail(data)
		if err != nil {
			return nil, err
		}
		return p.Children, nil
	}
	return email.UnrollArchive(data, name)
}

// AbsoluteDisplay resolves only recorded workspace-relative display paths.
func (r *Resolver) AbsoluteDisplay(display string) (string, error) {
	for _, root := range r.roots {
		rel := display
		if len(r.roots) > 1 {
			prefix := filepath.Base(root.name) + "/"
			if !strings.HasPrefix(rel, prefix) {
				continue
			}
			rel = strings.TrimPrefix(rel, prefix)
		}
		if validPath(rel) {
			return filepath.Join(root.name, filepath.FromSlash(rel)), nil
		}
	}
	return "", ErrUnsafe
}
