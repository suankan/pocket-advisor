package mcp

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"time"
	"unicode/utf8"
)

const (
	defaultSnapshotTTL       = 10 * time.Minute
	defaultMaxSnapshots      = 8
	defaultSnapshotStoreSize = 2 * 1024 * 1024
	resultIDRandomBytes      = 6
	cursorRandomBytes        = 24
	placeholderCursor        = "xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx"
)

type snapshotStore struct {
	snapshots map[string]*evidenceSnapshot
	cursors   map[string]*cursorState
	reserved  map[string]struct{}
	bytes     int
}

type evidenceSnapshot struct {
	result     *EvidenceResult
	index      []EvidenceIndexPacket
	items      []evidenceTextItem
	size       int
	lastAccess time.Time
	expires    time.Time
}

type evidenceTextItem struct {
	reference string
	relation  *string
	document  EvidenceDocument
	text      string
}

type cursorPosition struct {
	phase  string
	index  int
	offset int
}

type cursorState struct {
	snapshotID string
	position   cursorPosition
	nextToken  string
}

func (t *QueryTool) initializeStoreLocked() {
	if t.store == nil {
		t.store = &snapshotStore{
			snapshots: make(map[string]*evidenceSnapshot),
			cursors:   make(map[string]*cursorState),
			reserved:  make(map[string]struct{}),
		}
	}
	if t.now == nil {
		t.now = time.Now
	}
	if t.random == nil {
		t.random = rand.Reader
	}
	if t.snapshotTTL <= 0 {
		t.snapshotTTL = defaultSnapshotTTL
	}
	if t.maxSnapshots <= 0 {
		t.maxSnapshots = defaultMaxSnapshots
	}
	if t.maxSnapshotBytes <= 0 {
		t.maxSnapshotBytes = defaultSnapshotStoreSize
	}
}

func (t *QueryTool) newResultID() (string, error) {
	t.stateMu.Lock()
	defer t.stateMu.Unlock()
	t.initializeStoreLocked()
	for range 16 {
		raw := make([]byte, resultIDRandomBytes)
		if _, err := io.ReadFull(t.random, raw); err != nil {
			return "", fmt.Errorf("generate result identity: %w", err)
		}
		resultID := "R" + hex.EncodeToString(raw)
		_, stored := t.store.snapshots[resultID]
		_, reserved := t.store.reserved[resultID]
		if !stored && !reserved {
			t.store.reserved[resultID] = struct{}{}
			return resultID, nil
		}
	}
	return "", fmt.Errorf("generate unique result identity")
}

func (t *QueryTool) releaseResultID(resultID string) {
	t.stateMu.Lock()
	defer t.stateMu.Unlock()
	if t.store != nil {
		delete(t.store.reserved, resultID)
	}
}

func (t *QueryTool) newCursorLocked() (string, error) {
	for range 16 {
		raw := make([]byte, cursorRandomBytes)
		if _, err := io.ReadFull(t.random, raw); err != nil {
			return "", fmt.Errorf("generate continuation cursor: %w", err)
		}
		cursor := base64.RawURLEncoding.EncodeToString(raw)
		if _, exists := t.store.cursors[cursor]; !exists {
			return cursor, nil
		}
	}
	return "", fmt.Errorf("generate unique continuation cursor")
}

func snapshotFromEvidence(result *EvidenceResult) (*evidenceSnapshot, error) {
	encoded, err := json.Marshal(result)
	if err != nil {
		return nil, fmt.Errorf("size evidence snapshot: %w", err)
	}
	snapshot := &evidenceSnapshot{
		result: result,
		index:  make([]EvidenceIndexPacket, 0, len(result.Packets)),
		items:  make([]evidenceTextItem, 0, len(result.Packets)),
		size:   len(encoded),
	}
	for _, packet := range result.Packets {
		snapshot.index = append(snapshot.index, indexPacket(packet))
		if packet.Text != nil && *packet.Text != "" {
			snapshot.items = append(snapshot.items, evidenceTextItem{
				reference: packet.Reference, document: packet.Document, text: *packet.Text,
			})
		}
		for _, related := range packet.Related {
			if related.Text == nil || *related.Text == "" {
				continue
			}
			relation := related.Relation
			snapshot.items = append(snapshot.items, evidenceTextItem{
				reference: packet.Reference, relation: &relation,
				document: related.Document, text: *related.Text,
			})
		}
	}
	return snapshot, nil
}

func (t *QueryTool) storeSearchResult(result *EvidenceResult) (CallToolResult, error) {
	snapshot, err := snapshotFromEvidence(result)
	if err != nil {
		t.releaseResultID(result.ResultID)
		return CallToolResult{}, err
	}

	t.stateMu.Lock()
	defer t.stateMu.Unlock()
	t.initializeStoreLocked()
	delete(t.store.reserved, result.ResultID)
	if _, exists := t.store.snapshots[result.ResultID]; exists {
		return CallToolResult{}, fmt.Errorf("duplicate result identity")
	}
	now := t.now()
	t.cleanupExpiredLocked(now)
	if snapshot.size > t.maxSnapshotBytes {
		return CallToolResult{}, &resultSizeError{limit: t.maxSnapshotBytes}
	}
	for len(t.store.snapshots) >= t.maxSnapshots || t.store.bytes+snapshot.size > t.maxSnapshotBytes {
		if !t.evictOldestLocked() {
			break
		}
	}
	snapshot.lastAccess = now
	snapshot.expires = now.Add(t.snapshotTTL)
	t.store.snapshots[result.ResultID] = snapshot
	t.store.bytes += snapshot.size

	page, next, err := t.buildIndexPage(snapshot, 0, true)
	if err != nil {
		t.removeSnapshotLocked(result.ResultID)
		return CallToolResult{}, err
	}
	if next != nil {
		cursor, err := t.newCursorLocked()
		if err != nil {
			t.removeSnapshotLocked(result.ResultID)
			return CallToolResult{}, err
		}
		t.store.cursors[cursor] = &cursorState{snapshotID: result.ResultID, position: *next}
		applyContinuation(page, t.ReadName(), cursor)
	}
	resultPage, err := finalizePage(page)
	if err != nil {
		t.removeSnapshotLocked(result.ResultID)
		return CallToolResult{}, err
	}
	if page.Complete {
		t.removeSnapshotLocked(result.ResultID)
	}
	return resultPage, nil
}

func (t *QueryTool) readEvidencePage(ctx context.Context, cursor string) (CallToolResult, error) {
	if err := ctx.Err(); err != nil {
		return CallToolResult{}, err
	}
	t.stateMu.Lock()
	defer t.stateMu.Unlock()
	if err := ctx.Err(); err != nil {
		return CallToolResult{}, err
	}
	t.initializeStoreLocked()
	now := t.now()
	t.cleanupExpiredLocked(now)
	state, ok := t.store.cursors[cursor]
	if !ok {
		return CallToolResult{}, &argumentError{message: "continuation cursor is invalid or expired; run the search again"}
	}
	snapshot, ok := t.store.snapshots[state.snapshotID]
	if !ok || !now.Before(snapshot.expires) {
		delete(t.store.cursors, cursor)
		return CallToolResult{}, &argumentError{message: "continuation cursor is invalid or expired; run the search again"}
	}

	var page *EvidencePage
	var next *cursorPosition
	var err error
	switch state.position.phase {
	case "index":
		page, next, err = t.buildIndexPage(snapshot, state.position.index, false)
	case "text":
		page, next, err = t.buildTextPage(snapshot, state.position.index, state.position.offset)
	default:
		err = fmt.Errorf("invalid cursor phase")
	}
	if err != nil {
		return CallToolResult{}, err
	}
	if next != nil {
		if state.nextToken == "" {
			nextCursor, err := t.newCursorLocked()
			if err != nil {
				return CallToolResult{}, err
			}
			state.nextToken = nextCursor
			t.store.cursors[nextCursor] = &cursorState{snapshotID: state.snapshotID, position: *next}
		}
		applyContinuation(page, t.ReadName(), state.nextToken)
	}
	resultPage, err := finalizePage(page)
	if err != nil {
		return CallToolResult{}, err
	}
	snapshot.lastAccess = now
	snapshot.expires = now.Add(t.snapshotTTL)
	// Retain the terminal cursor until TTL/eviction so retrying the same cursor
	// is idempotent even when it names the final page.
	return resultPage, nil
}

func (t *QueryTool) buildIndexPage(snapshot *evidenceSnapshot, start int, search bool) (*EvidencePage, *cursorPosition, error) {
	kind := "index"
	var question *string
	var subQueries []string
	if search {
		kind = "search"
		question = &snapshot.result.Question
		subQueries = append([]string{}, snapshot.result.SubQueries...)
	} else {
		subQueries = []string{}
	}
	page := &EvidencePage{
		Kind: kind, ResultID: snapshot.result.ResultID, Question: question,
		SubQueries: subQueries, Packets: []EvidenceIndexPacket{}, Segments: []EvidenceSegment{},
		Warnings: append([]string{}, snapshot.result.Warnings...), DeliveryWarnings: []string{},
		EvidenceBudget: snapshot.result.Budget,
	}

	selected := start
	for selected < len(snapshot.index) {
		candidate := *page
		candidate.Packets = append(append([]EvidenceIndexPacket{}, page.Packets...), snapshot.index[selected])
		next := nextAfterIndex(snapshot, selected+1)
		setPlaceholderContinuation(&candidate, t.ReadName(), next != nil)
		if !pageFits(&candidate) {
			break
		}
		page.Packets = candidate.Packets
		selected++
	}
	if selected == start && selected < len(snapshot.index) {
		return nil, nil, &resultSizeError{limit: targetToolResultBytes}
	}
	next := nextAfterIndex(snapshot, selected)
	if next == nil {
		page.Complete = true
	}
	return page, next, nil
}

func nextAfterIndex(snapshot *evidenceSnapshot, index int) *cursorPosition {
	if index < len(snapshot.index) {
		return &cursorPosition{phase: "index", index: index}
	}
	if len(snapshot.items) > 0 {
		return &cursorPosition{phase: "text", index: 0}
	}
	return nil
}

func (t *QueryTool) buildTextPage(snapshot *evidenceSnapshot, itemIndex, offset int) (*EvidencePage, *cursorPosition, error) {
	if itemIndex < 0 || itemIndex >= len(snapshot.items) {
		return nil, nil, fmt.Errorf("cursor text item is out of range")
	}
	item := snapshot.items[itemIndex]
	if offset < 0 || offset >= len(item.text) || !utf8.RuneStart(item.text[offset]) {
		return nil, nil, fmt.Errorf("cursor text offset is invalid")
	}

	base := &EvidencePage{
		Kind: "text", ResultID: snapshot.result.ResultID, Question: nil,
		SubQueries: []string{}, Packets: []EvidenceIndexPacket{}, Segments: []EvidenceSegment{},
		Warnings: append([]string{}, snapshot.result.Warnings...), DeliveryWarnings: []string{},
		EvidenceBudget: snapshot.result.Budget,
	}
	low, high, best := 1, len(item.text)-offset, 0
	for low <= high {
		middle := low + (high-low)/2
		end := utf8BoundaryAtOrBefore(item.text, offset+middle)
		if end <= offset {
			end = nextUTF8Boundary(item.text, offset)
		}
		candidate := textPageCandidate(base, item, offset, end)
		next := nextAfterText(snapshot, itemIndex, end)
		setPlaceholderContinuation(candidate, t.ReadName(), next != nil)
		if pageFits(candidate) {
			best = end
			low = middle + 1
		} else {
			high = middle - 1
		}
	}
	if best <= offset {
		return nil, nil, &resultSizeError{limit: targetToolResultBytes}
	}
	if paragraphEnd := preferredParagraphEnd(item.text, offset, best); paragraphEnd > offset {
		best = paragraphEnd
	}
	page := textPageCandidate(base, item, offset, best)
	next := nextAfterText(snapshot, itemIndex, best)
	if next == nil {
		page.Complete = true
	}
	return page, next, nil
}

func textPageCandidate(base *EvidencePage, item evidenceTextItem, start, end int) *EvidencePage {
	page := *base
	page.Segments = []EvidenceSegment{{
		Reference: item.reference, Relation: item.relation, Document: item.document,
		Text:         item.text[start:end],
		Range:        EvidenceRange{Start: start, End: end, Unit: utf8ByteUnit},
		TextComplete: end == len(item.text),
	}}
	return &page
}

func nextAfterText(snapshot *evidenceSnapshot, itemIndex, end int) *cursorPosition {
	if end < len(snapshot.items[itemIndex].text) {
		return &cursorPosition{phase: "text", index: itemIndex, offset: end}
	}
	if itemIndex+1 < len(snapshot.items) {
		return &cursorPosition{phase: "text", index: itemIndex + 1}
	}
	return nil
}

func applyContinuation(page *EvidencePage, toolName, cursor string) {
	page.Complete = false
	page.NextCursor = &cursor
	page.ContinuationTool = &toolName
	page.DeliveryWarnings = []string{deliveryMoreAvailable}
}

func setPlaceholderContinuation(page *EvidencePage, toolName string, incomplete bool) {
	if !incomplete {
		page.Complete = true
		page.NextCursor = nil
		page.ContinuationTool = nil
		page.DeliveryWarnings = []string{}
		return
	}
	applyContinuation(page, toolName, placeholderCursor)
}

func utf8BoundaryAtOrBefore(value string, end int) int {
	if end >= len(value) {
		return len(value)
	}
	if end < 0 {
		return 0
	}
	for end > 0 && !utf8.RuneStart(value[end]) {
		end--
	}
	return end
}

func nextUTF8Boundary(value string, start int) int {
	_, size := utf8.DecodeRuneInString(value[start:])
	if size <= 0 {
		return start
	}
	return start + size
}

func preferredParagraphEnd(value string, start, maximum int) int {
	segment := value[start:maximum]
	index := strings.LastIndex(segment, "\n\n")
	if index < len(segment)/2 {
		return 0
	}
	return start + index + 2
}

func (t *QueryTool) cleanupExpiredLocked(now time.Time) {
	for resultID, snapshot := range t.store.snapshots {
		if !now.Before(snapshot.expires) {
			t.removeSnapshotLocked(resultID)
		}
	}
}

func (t *QueryTool) evictOldestLocked() bool {
	var oldestID string
	var oldest time.Time
	for resultID, snapshot := range t.store.snapshots {
		if oldestID == "" || snapshot.lastAccess.Before(oldest) {
			oldestID = resultID
			oldest = snapshot.lastAccess
		}
	}
	if oldestID == "" {
		return false
	}
	t.removeSnapshotLocked(oldestID)
	return true
}

func (t *QueryTool) removeSnapshotLocked(resultID string) {
	snapshot, exists := t.store.snapshots[resultID]
	if !exists {
		return
	}
	delete(t.store.snapshots, resultID)
	t.store.bytes -= snapshot.size
	for cursor, state := range t.store.cursors {
		if state.snapshotID == resultID {
			delete(t.store.cursors, cursor)
		}
	}
}

func (t *QueryTool) closeSnapshots() {
	t.stateMu.Lock()
	defer t.stateMu.Unlock()
	if t.store == nil {
		return
	}
	t.store = &snapshotStore{
		snapshots: make(map[string]*evidenceSnapshot), cursors: make(map[string]*cursorState), reserved: make(map[string]struct{}),
	}
}
