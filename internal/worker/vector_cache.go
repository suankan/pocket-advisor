package worker

import (
	"container/list"
	"crypto/sha256"
	"sync"
)

// VectorCache keeps the vectors of recent embedding requests, keyed by model
// and passage text, so a document whose job is retried — after a timeout
// part-way through, say — resumes from the batches it already has instead of
// embedding them again. A document's passages are stored only when every one
// has a vector, so without it a late failure throws all of them away, and a
// document needing longer than one handler's deadline could never finish.
//
// It lives in the process: a restart starts it empty. It is bounded by the
// bytes its vectors take, oldest out first.
type VectorCache struct {
	mu      sync.Mutex
	max     int
	size    int
	entries map[vectorKey]*list.Element
	order   *list.List
}

type vectorKey struct {
	model string
	text  [sha256.Size]byte
}

type vectorEntry struct {
	key    vectorKey
	vector []float32
}

// NewVectorCache holds vectors up to maxBytes.
func NewVectorCache(maxBytes int) *VectorCache {
	return &VectorCache{max: maxBytes, entries: map[vectorKey]*list.Element{}, order: list.New()}
}

func keyOf(model, text string) vectorKey {
	return vectorKey{model: model, text: sha256.Sum256([]byte(text))}
}

// Get returns the vector cached for text under model. A nil cache has none.
func (c *VectorCache) Get(model, text string) ([]float32, bool) {
	if c == nil {
		return nil, false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.entries[keyOf(model, text)]
	if !ok {
		return nil, false
	}
	c.order.MoveToFront(e)
	return e.Value.(*vectorEntry).vector, true
}

// Put caches vector for text under model, evicting the least recently used
// vectors beyond the bound. A nil cache keeps nothing.
func (c *VectorCache) Put(model, text string, vector []float32) {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	k := keyOf(model, text)
	if e, ok := c.entries[k]; ok {
		c.order.MoveToFront(e)
		return
	}
	c.entries[k] = c.order.PushFront(&vectorEntry{key: k, vector: vector})
	c.size += 4 * len(vector)
	for c.size > c.max && c.order.Len() > 0 {
		oldest := c.order.Back()
		v := c.order.Remove(oldest).(*vectorEntry)
		delete(c.entries, v.key)
		c.size -= 4 * len(v.vector)
	}
}
