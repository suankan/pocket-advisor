//go:build manual

package worker

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/suankan/pocket-advisor/internal/bus"
)

// NATS_TEST_URL must point at a disposable broker. No workspace config is read.
func TestShutdownRemainsDeliverableBeyondBrokerLimit(t *testing.T) {
	url := os.Getenv("NATS_TEST_URL")
	if url == "" {
		t.Skip("set NATS_TEST_URL to a disposable broker")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	ws := fmt.Sprintf("shutdown-%d", time.Now().UnixNano())
	b, err := bus.Connect(ctx, url, ws)
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	if err := b.EnsureStreams(ctx); err != nil {
		t.Fatal(err)
	}
	nc, err := nats.Connect(url)
	if err != nil {
		t.Fatal(err)
	}
	defer nc.Close()
	js, err := jetstream.New(nc)
	if err != nil {
		t.Fatal(err)
	}
	suffix := strings.ToUpper(strings.ReplaceAll(ws, "-", "_"))
	defer func() {
		for _, name := range []string{bus.StreamName, bus.StreamDLQ} {
			_ = js.DeleteStream(context.Background(), name+"_"+suffix)
		}
	}()
	stream := bus.StreamName + "_" + suffix
	subject := ws + "." + bus.SubjectEmails
	// Exercise the upgrade of an existing three-delivery durable consumer.
	if _, err := js.CreateOrUpdateConsumer(ctx, stream, jetstream.ConsumerConfig{
		Durable: "shutdown-test", FilterSubject: subject,
		AckPolicy: jetstream.AckExplicitPolicy, MaxDeliver: 3,
	}); err != nil {
		t.Fatal(err)
	}
	consumer, err := b.PullConsumer(ctx, bus.StreamName, "shutdown-test", bus.SubjectEmails)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := js.Publish(ctx, subject, []byte("synthetic work")); err != nil {
		t.Fatal(err)
	}
	rt := &Runtime{Name: "shutdown-test", Subject: bus.SubjectEmails, Bus: b, Log: quietLogger()}
	for i := 1; i <= bus.MaxDeliver+2; i++ {
		msg, err := consumer.Next(jetstream.FetchMaxWait(time.Second))
		if err != nil {
			t.Fatalf("delivery %d was stranded: %v", i, err)
		}
		meta, err := msg.Metadata()
		if err != nil {
			t.Fatal(err)
		}
		if meta.NumDelivered != uint64(i) {
			t.Fatalf("delivery=%d want %d", meta.NumDelivered, i)
		}
		stopping, stop := context.WithCancel(ctx)
		stop()
		rt.handle(stopping, msg, func(context.Context, jetstream.Msg) error { return errors.New("shutdown") })
	}
	msg, err := consumer.Next(jetstream.FetchMaxWait(time.Second))
	if err != nil {
		t.Fatalf("work did not survive shutdowns: %v", err)
	}
	rt.handle(ctx, msg, func(context.Context, jetstream.Msg) error { return nil })
	work, err := b.StreamInfo(ctx, bus.StreamName)
	if err != nil {
		t.Fatal(err)
	}
	if work.State.Msgs != 0 {
		t.Fatalf("successful work still pending: %d messages", work.State.Msgs)
	}
	info, err := b.StreamInfo(ctx, bus.StreamDLQ)
	if err != nil {
		t.Fatal(err)
	}
	if info.State.Msgs != 0 {
		t.Fatalf("shutdown produced %d dead letters", info.State.Msgs)
	}
}
