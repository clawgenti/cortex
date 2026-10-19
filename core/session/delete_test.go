package session

import (
	"testing"

	"github.com/rossoctl/cortex/core/pipeline"
)

// deleteSpy is a Recorder that is also a Deleter. It notes whether the store still held the id
// at the moment it was told, which must be no: a Deleter resets state keyed by the id, and an
// entry still resident could be appended to between the two halves.
type deleteSpy struct {
	s       *Store
	deleted []string
	heldAt  []bool
}

func (d *deleteSpy) Record(string, *pipeline.SessionEvent) {}
func (d *deleteSpy) Deleted(id string) {
	d.deleted = append(d.deleted, id)
	_, held := d.s.sessions[id] // s.mu is held: Deleted runs inside Delete's critical section
	d.heldAt = append(d.heldAt, held)
}

// Delete removes the one session and every map that names it, and leaves the others alone.
func TestDelete_RemovesOneSessionAndItsBookkeeping(t *testing.T) {
	s, clk := newProcStore()
	defer s.Close()
	s.Append("pending:bob-shell", ev())
	s.Claim("task-1", "bob-shell") // an owner, and an adoption of the pending bucket
	recordIn(s, clk, "s2")
	recordIn(s, clk, "task-1") // the active session now

	if !s.Delete("task-1") {
		t.Fatal("Delete(task-1) = false, want true: memory held it")
	}
	if s.View("task-1") != nil {
		t.Fatal("task-1 is still in the store")
	}
	if s.View("s2") == nil {
		t.Fatal("Delete(task-1) removed s2 too")
	}
	// The field, not ActiveSession(): that already hides an id with no entry.
	if s.activeID == "task-1" {
		t.Fatal("activeID still names the deleted session")
	}
	if _, ok := s.owners["task-1"]; ok {
		t.Fatal("task-1's owner survived")
	}
	for p, to := range s.adopted {
		if p == "task-1" || to == "task-1" {
			t.Fatalf("adoption %s -> %s survived", p, to)
		}
	}
	// The adoption is gone, so a straggler under the pending id is its own bucket again rather
	// than re-creating task-1.
	s.Append("pending:bob-shell", ev())
	if s.View("task-1") != nil || s.View("pending:bob-shell") == nil {
		t.Fatal("a straggler followed an adoption Delete should have forgotten")
	}
}

// Every Deleter is told, inside the critical section and after the store dropped the session —
// and for an id memory never held too, since the archive and the usage ring can hold one alone.
func TestDelete_TellsDeletersEvenForAnIDMemoryNeverHeld(t *testing.T) {
	s := New(0, 0, 0)
	defer s.Close()
	spy := &deleteSpy{s: s}
	s.AddRecorder(spy)
	s.Append("a", ev())
	if !s.Delete("a") {
		t.Fatal("Delete(a) = false")
	}
	if s.Delete("archived-only") {
		t.Fatal("Delete(archived-only) = true for an id memory never held")
	}
	if len(spy.deleted) != 2 || spy.deleted[0] != "a" || spy.deleted[1] != "archived-only" {
		t.Fatalf("Deleted calls = %v, want [a archived-only]", spy.deleted)
	}
	if spy.heldAt[0] {
		t.Fatal("the store still held a when it told its Deleters")
	}
}

// The next append to a deleted id starts a new session, numbered after whatever a SeqSeeder
// still holds for it: until the archive has removed its files, their seqs must not be reused.
func TestDelete_TheNextAppendIsNumberedAfterTheSeeder(t *testing.T) {
	s := New(0, 0, 0)
	defer s.Close()
	s.AddRecorder(newFakeArchive()) // a SeqSeeder, not a Deleter: it keeps a's last seq, 3
	for range 3 {
		s.Append("a", ev())
	}
	s.Delete("a")
	s.Append("a", ev())
	if got := seqsOf(t, s, "a"); len(got) != 1 || got[0] != 4 {
		t.Fatalf("a after Delete = %v, want one event numbered 4", got)
	}
}

// A Delete of an id memory does not hold changes nothing in memory. An adopted pending id is
// the case that matters: its bucket is gone, but its adoption still redirects the response that
// was in flight when the pending bucket was adopted. Dropping the adoption would let that
// response re-create the pending bucket as a stray row.
func TestDelete_AnIDMemoryDoesNotHoldKeepsItsAdoption(t *testing.T) {
	s, _ := newProcStore()
	defer s.Close()
	s.Append("pending:bob-shell", ev())
	s.Claim("task-1", "bob-shell") // adopts the pending bucket into task-1
	before := len(s.View("task-1").Events)

	if s.Delete("pending:bob-shell") {
		t.Fatal("Delete(pending:bob-shell) = true, want false: the bucket was adopted away")
	}
	s.Append("pending:bob-shell", ev()) // the straggler the adoption exists to redirect
	if s.View("pending:bob-shell") != nil {
		t.Fatal("the straggler re-created the pending bucket: Delete dropped the adoption")
	}
	if got := len(s.View("task-1").Events); got != before+1 {
		t.Fatalf("task-1 holds %d events, want %d: the straggler did not follow the adoption", got, before+1)
	}
}
