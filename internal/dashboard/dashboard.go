// Package dashboard renders live run state to the terminal.
//
// It exists because the monolith took something away. Six roles that each had a
// pod and a `kubectl logs` stream now share one stdout, and interleaving them
// produces noise rather than insight. The log files keep the detail; this shows
// the shape — how much is queued, how many lanes are busy, and whether the
// machine is actually saturated.
//
// Hand-rolled ANSI rather than a TUI framework: the layout is fixed and nothing
// here is interactive, so an event loop and a widget tree would be unused
// weight on top of a repaint.
package dashboard

import (
	"context"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/suankan/pocket-advisor/internal/limits"
	"github.com/suankan/pocket-advisor/internal/telemetry"
)

// EmbedGauge is the embedding client's live state.
type EmbedGauge interface {
	InFlight() int
	Concurrency() int
	BreakerState() string
}

// DBGauge is the Postgres pool's live state.
type DBGauge interface {
	PoolStats() (acquired, max int32)
}

// Source is everything the dashboard reads. It never mutates any of it.
type Source struct {
	Stats     *telemetry.Stats
	CPU       *limits.CPU
	Embedder  EmbedGauge
	DB        DBGauge
	Mode      string
	Workspace string
	LogDir    string
}

const (
	refresh  = 250 * time.Millisecond
	plainInt = 5 * time.Second
	barWidth = 22
)

type Dashboard struct {
	src   Source
	out   io.Writer
	tty   bool
	color bool
	start time.Time

	lines int
	prev  map[string]rateSample
}

type rateSample struct {
	completed int64
	at        time.Time
}

// New builds a dashboard for out.
//
// A non-terminal destination (a pipe, a redirect, CI) gets periodic one-line
// summaries instead: repaint escapes written to a file are unreadable garbage,
// and progress still has to be legible there.
func New(src Source, out *os.File) *Dashboard {
	return &Dashboard{
		src:   src,
		out:   out,
		tty:   isTerminal(out),
		color: isTerminal(out) && os.Getenv("NO_COLOR") == "",
		start: time.Now(),
		prev:  make(map[string]rateSample),
	}
}

func isTerminal(f *os.File) bool {
	info, err := f.Stat()
	if err != nil {
		return false
	}
	return info.Mode()&os.ModeCharDevice != 0
}

// Run repaints until ctx is cancelled, then leaves a final frame in place so
// the terminal keeps the last state rather than a half-erased one.
func (d *Dashboard) Run(ctx context.Context) {
	interval := refresh
	if !d.tty {
		interval = plainInt
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	d.paint()
	for {
		select {
		case <-ctx.Done():
			d.paint()
			d.finish()
			return
		case <-ticker.C:
			d.paint()
		}
	}
}

func (d *Dashboard) paint() {
	if !d.tty {
		fmt.Fprintln(d.out, d.oneLine())
		return
	}
	frame := d.render()
	d.clear()
	fmt.Fprint(d.out, frame)
	d.lines = strings.Count(frame, "\n")
}

// clear rewinds over the previous frame. Every line is erased explicitly rather
// than overwritten, so a frame that shrinks does not leave the tail of a longer
// one behind it.
func (d *Dashboard) clear() {
	if d.lines == 0 {
		return
	}
	var b strings.Builder
	for i := 0; i < d.lines; i++ {
		b.WriteString("\033[1A\033[2K")
	}
	b.WriteString("\r")
	fmt.Fprint(d.out, b.String())
}

func (d *Dashboard) finish() {
	if d.tty {
		fmt.Fprintln(d.out)
	}
}

func (d *Dashboard) render() string {
	var b strings.Builder

	header := fmt.Sprintf("pocket-advisor · %s", d.src.Mode)
	if d.src.Workspace != "" {
		header += " · workspace=" + d.src.Workspace
	}
	header += " · elapsed " + elapsed(time.Since(d.start))
	fmt.Fprintf(&b, "%s\n\n", d.bold(header))

	d.renderUpload(&b)
	d.renderQueues(&b)
	d.renderResources(&b)

	if d.src.LogDir != "" {
		fmt.Fprintf(&b, "\n%s\n", d.dim("logs → "+d.src.LogDir+"/<role>.log"))
	}
	return b.String()
}

func (d *Dashboard) renderUpload(b *strings.Builder) {
	u := d.src.Stats.Upload.Snapshot()
	if u.Total == 0 && u.Done() == 0 {
		return
	}

	frac := 0.0
	if u.Total > 0 {
		frac = float64(u.Done()) / float64(u.Total)
	}
	status := fmt.Sprintf("%s/%s files · %s · %s dup",
		commas(u.Done()), commas(u.Total), bytesHuman(u.Bytes), commas(u.Duplicate))
	if u.Failed > 0 {
		status += " · " + d.red(fmt.Sprintf("%s failed", commas(u.Failed)))
	}
	fmt.Fprintf(b, "%-9s %s  %s\n\n", d.bold("UPLOAD"), bar(frac, barWidth), status)
}

func (d *Dashboard) renderQueues(b *strings.Builder) {
	snaps := d.src.Stats.QueueSnapshots()
	if len(snaps) == 0 {
		return
	}

	fmt.Fprintf(b, "%s\n", d.bold(fmt.Sprintf(
		"%-20s %8s %9s %8s %6s %6s %5s %8s",
		"QUEUE", "pending", "workers", "done", "skip", "retry", "dlq", "rate")))

	now := time.Now()
	for _, s := range snaps {
		rate := d.rate(s, now)
		dlq := commas(s.DLQ)
		if s.DLQ > 0 {
			dlq = d.red(dlq)
		}
		fmt.Fprintf(b, "  %-18s %8s %9s %8s %6s %6s %5s %8s\n",
			short(s.Subject),
			commas(s.Pending),
			fmt.Sprintf("%d/%d", s.Active, s.Lanes),
			commas(s.Completed),
			commas(s.Skipped),
			commas(s.Retried),
			dlq,
			rate)
	}
	fmt.Fprintln(b)
}

// rate is measured between repaints rather than averaged over the run, so a
// stage that has stalled reads as stalled instead of coasting on its history.
func (d *Dashboard) rate(s telemetry.QueueSnapshot, now time.Time) string {
	prev, ok := d.prev[s.Subject]
	d.prev[s.Subject] = rateSample{completed: s.Completed, at: now}
	if !ok {
		return "-"
	}
	secs := now.Sub(prev.at).Seconds()
	if secs <= 0 {
		return "-"
	}
	delta := float64(s.Completed - prev.completed)
	if delta <= 0 {
		return "-"
	}
	return fmt.Sprintf("%.1f/s", delta/secs)
}

func (d *Dashboard) renderResources(b *strings.Builder) {
	if c := d.src.CPU; c != nil {
		frac := float64(c.InUse()) / float64(c.Size())
		fmt.Fprintf(b, "%-11s %s  %d/%d busy · ocr %d · rasterize %d\n",
			d.bold("CPU POOL"), bar(frac, 12), c.InUse(), c.Size(),
			c.Active(limits.LabelOCR), c.Active(limits.LabelRasterize))
	}

	if e := d.src.Embedder; e != nil {
		conc := e.Concurrency()
		frac := 0.0
		if conc > 0 {
			frac = float64(e.InFlight()) / float64(conc)
		}
		state := e.BreakerState()
		if state != "closed" {
			state = d.red("breaker " + state)
		} else {
			state = "breaker closed"
		}
		fmt.Fprintf(b, "%-11s %s  %d/%d sessions · %s\n",
			d.bold("EMBEDDING"), bar(frac, 12), e.InFlight(), conc, state)
	}

	if db := d.src.DB; db != nil {
		acquired, maxConns := db.PoolStats()
		frac := 0.0
		if maxConns > 0 {
			frac = float64(acquired) / float64(maxConns)
		}
		fmt.Fprintf(b, "%-11s %s  %d/%d conns\n",
			d.bold("POSTGRES"), bar(frac, 12), acquired, maxConns)
	}
}

// oneLine is the non-TTY form: no cursor movement, one appendable line.
func (d *Dashboard) oneLine() string {
	u := d.src.Stats.Upload.Snapshot()
	var parts []string
	parts = append(parts, "["+elapsed(time.Since(d.start))+"]")
	if u.Total > 0 {
		parts = append(parts, fmt.Sprintf("upload %d/%d", u.Done(), u.Total))
	}
	for _, s := range d.src.Stats.QueueSnapshots() {
		parts = append(parts, fmt.Sprintf("%s pending=%d active=%d done=%d dlq=%d",
			short(s.Subject), s.Pending, s.Active, s.Completed, s.DLQ))
	}
	if c := d.src.CPU; c != nil {
		parts = append(parts, fmt.Sprintf("cpu=%d/%d", c.InUse(), c.Size()))
	}
	return strings.Join(parts, " | ")
}

// Summary is the plain closing report, printed after the dashboard tears down
// so the outcome survives on a scrollback that no longer repaints.
func (d *Dashboard) Summary() string {
	var b strings.Builder

	u := d.src.Stats.Upload.Snapshot()
	if u.Total > 0 {
		fmt.Fprintf(&b, "upload    %s uploaded · %s duplicate · %s failed · %s\n",
			commas(u.Uploaded), commas(u.Duplicate), commas(u.Failed), bytesHuman(u.Bytes))
	}
	for _, s := range d.src.Stats.QueueSnapshots() {
		fmt.Fprintf(&b, "%-9s %s done · %s skipped · %s dead-lettered\n",
			short(s.Subject), commas(s.Completed), commas(s.Skipped), commas(s.DLQ))
	}
	fmt.Fprintf(&b, "elapsed   %s\n", elapsed(time.Since(d.start)))
	return b.String()
}

// --- formatting ------------------------------------------------------------

func bar(frac float64, width int) string {
	if frac < 0 {
		frac = 0
	}
	if frac > 1 {
		frac = 1
	}
	filled := int(frac*float64(width) + 0.5)
	return strings.Repeat("█", filled) + strings.Repeat("░", width-filled)
}

// short trims the subject to the part that differs, since every subject shares
// the ingest. prefix and the column is narrow.
func short(subject string) string {
	return strings.TrimPrefix(subject, "ingest.")
}

func elapsed(d time.Duration) string {
	d = d.Round(time.Second)
	h := int(d.Hours())
	m := int(d.Minutes()) % 60
	s := int(d.Seconds()) % 60
	return fmt.Sprintf("%02d:%02d:%02d", h, m, s)
}

func commas(n int64) string {
	s := fmt.Sprintf("%d", n)
	if n < 0 || len(s) <= 3 {
		return s
	}
	var out []byte
	for i, c := range []byte(s) {
		if i > 0 && (len(s)-i)%3 == 0 {
			out = append(out, ',')
		}
		out = append(out, c)
	}
	return string(out)
}

func bytesHuman(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for v := n / unit; v >= unit && exp < 3; v /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(n)/float64(div), "KMGT"[exp])
}

func (d *Dashboard) bold(s string) string { return d.wrap(s, "\033[1m") }
func (d *Dashboard) dim(s string) string  { return d.wrap(s, "\033[2m") }
func (d *Dashboard) red(s string) string  { return d.wrap(s, "\033[31m") }

func (d *Dashboard) wrap(s, code string) string {
	if !d.color {
		return s
	}
	return code + s + "\033[0m"
}
