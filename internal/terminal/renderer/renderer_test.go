package renderer

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"unicode/utf8"

	"j0s.at/vibeshell/internal/domain"
	"j0s.at/vibeshell/internal/terminal/screen"
)

// memoryStore is a content store double: it keeps exact bytes, hands out
// distinct identifiers per store, and can be told to fail or to lie about a
// size so the renderer's own checks are exercised.
type memoryStore struct {
	blobs     map[string][]byte
	next      int
	mediaType string
	putErr    error
	getErr    error
	getCalls  int
	sizeDelta int64
}

func newMemoryStore() *memoryStore {
	return &memoryStore{blobs: map[string][]byte{}}
}

func (s *memoryStore) Put(_ context.Context, data []byte, mediaType string) (domain.ContentRef, error) {
	if s.putErr != nil {
		return domain.EmptyContentRef(), s.putErr
	}
	s.next++
	hash, err := domain.ParseContentID(fmt.Sprintf("cnt_%026d", s.next))
	if err != nil {
		return domain.EmptyContentRef(), err
	}
	stored := append([]byte(nil), data...)
	s.blobs[hash.String()] = stored
	s.mediaType = mediaType
	return domain.ContentRef{Hash: hash, Size: int64(len(stored)) + s.sizeDelta, MediaType: mediaType}, nil
}

func (s *memoryStore) Get(_ context.Context, ref domain.ContentRef, offset, length int64) ([]byte, error) {
	s.getCalls++
	if s.getErr != nil {
		return nil, s.getErr
	}
	data, ok := s.blobs[ref.Hash.String()]
	if !ok {
		return nil, domain.NewNotFoundError(domain.CodeContentNotFound, "no such content", nil)
	}
	if offset > int64(len(data)) {
		offset = int64(len(data))
	}
	end := min(offset+length, int64(len(data)))
	return append([]byte(nil), data[offset:end]...), nil
}

// stored returns the bytes a reference names.
func (s *memoryStore) stored(t *testing.T, ref domain.ContentRef) []byte {
	t.Helper()
	data, ok := s.blobs[ref.Hash.String()]
	if !ok {
		t.Fatalf("no stored content for %s", ref.Hash)
	}
	return data
}

// testSession is a fixed identity for the renderer calls.
func testSession(t *testing.T) domain.SessionID {
	t.Helper()
	id, err := domain.ParseSessionID("ses_01ARZ3NDEKTSV4RRFFQ69G5FAV")
	if err != nil {
		t.Fatalf("ParseSessionID: %v", err)
	}
	return id
}

func newTestRenderer(t *testing.T, store *memoryStore) *Renderer {
	t.Helper()
	renderer, err := New(store, DefaultLimits())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return renderer
}

// put stores buffer content and returns its reference.
func put(t *testing.T, store *memoryStore, content string) *domain.ContentRef {
	t.Helper()
	ref, err := store.Put(context.Background(), []byte(content), "text/plain; charset=utf-8")
	if err != nil {
		t.Fatalf("Put: %v", err)
	}
	return &ref
}

// assertOnlyRendererSequences removes the sequences the renderer is allowed to
// emit and requires that nothing else in the frame starts with an escape. This
// is the invariant that data text can never activate a terminal control.
func assertOnlyRendererSequences(t *testing.T, frame []byte) {
	t.Helper()
	rest := frame
	for _, sequence := range []string{
		screen.EnterAlternateScreen, screen.LeaveAlternateScreen, screen.ResetStyle,
		screen.CursorHome, screen.EraseLine, screen.ShowCursor, screen.HideCursor,
	} {
		rest = []byte(strings.ReplaceAll(string(rest), sequence, ""))
	}
	// Cursor positioning, style changes, and the painter's sequences all match
	// ESC [ <parameters> <final>.
	for {
		index := strings.IndexByte(string(rest), 0x1b)
		if index < 0 {
			break
		}
		tail := string(rest[index:])
		if len(tail) < 3 || tail[1] != '[' {
			t.Fatalf("frame holds a sequence the renderer does not emit: %q", tail)
		}
		end := 2
		for end < len(tail) && (tail[end] < 0x40 || tail[end] > 0x7e) {
			end++
		}
		if end >= len(tail) {
			t.Fatalf("frame holds an unfinished sequence: %q", tail)
		}
		rest = []byte(tail[end+1:])
	}
	if !utf8.Valid(rest) {
		t.Fatalf("frame is not valid UTF-8, so a raw control byte from data text survived")
	}
	for _, r := range string(rest) {
		if r >= 0x80 && r <= 0x9f {
			t.Fatalf("frame holds the control character %U from data text", r)
		}
	}
}
