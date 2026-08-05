//go:build manual

package bus

import (
	"context"
	"fmt"
	"os"
	"sort"
	"testing"
	"time"

	"github.com/nats-io/nats.go/jetstream"

	"github.com/suankan/pocket-advisor/internal/config"
)

// Reads the DLQ without consuming it: an ephemeral, explicit-ack consumer that
// vanishes afterwards, so it cannot disturb the position a durable redrive
// consumer would keep.
func TestListDLQ(t *testing.T) {
	ws := os.Getenv("WORKSPACE_ID")
	if ws == "" {
		t.Skip("set WORKSPACE_ID")
	}
	cfg, err := config.Load(os.Getenv("CONFIG_PATH"))
	if err != nil {
		t.Fatal(err)
	}
	w, err := cfg.Workspace(ws)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	b, err := Connect(ctx, w.NATSURL, w.NATSUser, w.NATSPassword)
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()

	cons, err := b.js.CreateConsumer(ctx, StreamDLQ, jetstream.ConsumerConfig{
		AckPolicy:     jetstream.AckNonePolicy,
		DeliverPolicy: jetstream.DeliverAllPolicy,
		FilterSubject: SubjectDLQ,
	})
	if err != nil {
		t.Fatal(err)
	}

	type key struct{ reason, worker, subject string }
	counts := map[key]int{}
	total := 0
	for {
		batch, err := cons.Fetch(200, jetstream.FetchMaxWait(2*time.Second))
		if err != nil {
			break
		}
		n := 0
		for m := range batch.Messages() {
			h := m.Headers()
			counts[key{h.Get(HdrFailureReason), h.Get(HdrFailureWorker), h.Get(HdrOrigSubject)}]++
			n++
			total++
		}
		if n == 0 {
			break
		}
	}

	rows := make([]key, 0, len(counts))
	for k := range counts {
		rows = append(rows, k)
	}
	sort.Slice(rows, func(i, j int) bool { return counts[rows[i]] > counts[rows[j]] })

	fmt.Printf("\n%-24s %-22s %-22s %s\n", "REASON", "WORKER", "ORIGINAL SUBJECT", "COUNT")
	for _, k := range rows {
		fmt.Printf("%-24s %-22s %-22s %d\n", k.reason, k.worker, k.subject, counts[k])
	}
	fmt.Printf("%-24s %-22s %-22s %d\n", "TOTAL", "", "", total)
}
