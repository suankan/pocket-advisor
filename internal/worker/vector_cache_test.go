package worker

import "testing"

// The cache returns what it was given under the same model and text only, and
// evicts the least recently used vectors beyond its bound.
func TestVectorCacheIsBoundedAndKeyedByModel(t *testing.T) {
	c := NewVectorCache(2 * 4 * 3) // two vectors of three dimensions
	c.Put("m", "a", []float32{1, 2, 3})
	c.Put("m", "b", []float32{4, 5, 6})
	if _, ok := c.Get("other", "a"); ok {
		t.Error("a vector was returned for another model")
	}
	if _, ok := c.Get("m", "a"); !ok {
		t.Fatal("a cached vector was not returned")
	}
	c.Put("m", "c", []float32{7, 8, 9})
	if _, ok := c.Get("m", "b"); ok {
		t.Error("the least recently used vector survived past the bound")
	}
	if v, ok := c.Get("m", "a"); !ok || v[0] != 1 {
		t.Errorf("a recently used vector was evicted: %v %v", v, ok)
	}
	var none *VectorCache
	none.Put("m", "a", []float32{1})
	if _, ok := none.Get("m", "a"); ok {
		t.Error("a nil cache returned a vector")
	}
}
