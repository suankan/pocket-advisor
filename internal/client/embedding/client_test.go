package embedding

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/suankan/pocket-advisor/internal/config"
)

// The endpoint's own token count comes back with the vectors, from
// prompt_tokens or else total_tokens; an endpoint that reports none gives 0.
func TestEmbedCountedReportsTheEndpointsTokens(t *testing.T) {
	for usage, want := range map[string]int{
		`,"usage":{"prompt_tokens":17,"total_tokens":17}`: 17,
		`,"usage":{"total_tokens":9}`:                     9,
		``:                                                0,
	} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte(`{"data":[{"index":0,"embedding":[0.5,0.5]}],"model":"m"` + usage + `}`))
		}))
		c := New(config.Embedding{Endpoint: srv.URL, Model: "m", Timeout: 5 * time.Second, Concurrency: 1})
		vectors, tokens, err := c.EmbedCounted(context.Background(), []string{"synthetic"})
		srv.Close()
		if err != nil || len(vectors) != 1 || tokens != want {
			t.Errorf("usage %q: %d vectors, %d tokens, %v; want %d tokens", usage, len(vectors), tokens, err, want)
		}
	}
}
