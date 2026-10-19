package session

import (
	"fmt"
	"runtime"
	"strings"
	"testing"

	"github.com/rossoctl/cortex/core/pipeline"
)

// benchTurns is a session long enough for the quadratic term to dominate, short enough
// to run in a second.
const benchTurns = 300

// benchConversation builds the messages one turn re-sends: the whole conversation so
// far. Rebuilt per turn, as a parser does, so nothing is shared by accident.
func benchConversation(turn int) []pipeline.InferenceMessage {
	out := make([]pipeline.InferenceMessage, 0, turn)
	for i := 0; i < turn; i++ {
		out = append(out, pipeline.InferenceMessage{
			Role:    "user",
			Content: fmt.Sprintf("message %04d: %s", i, strings.Repeat("prompt text ", 40)),
		})
	}
	return out
}

// benchToolManifest builds the tool manifest a client re-sends on every request, sized
// from a live session: 27 tools, ~800 bytes of schema each. Rebuilt per call, as a parser
// does, so nothing is shared by accident.
//
// nonce distinguishes the two sub-benchmarks below. Passing the turn number makes every
// turn's manifest unique, which is what interning cannot collapse.
func benchToolManifest(nonce int) []pipeline.InferenceTool {
	out := make([]pipeline.InferenceTool, 0, 27)
	for i := 0; i < 27; i++ {
		out = append(out, pipeline.InferenceTool{
			Name:        fmt.Sprintf("tool_%02d", i),
			Description: fmt.Sprintf("tool %02d: %s", i, strings.Repeat("what it does ", 20)),
			Parameters: pipeline.RawJSON(fmt.Sprintf(
				`{"type": "object", "nonce": %d, "properties": {"arg_%02d": {"type": "string", "description": "%s"}}}`,
				nonce, i, strings.Repeat("an argument ", 40))),
		})
	}
	return out
}

// BenchmarkRetainedHeapWithTools reports what the tool manifest costs a session, which is
// the term BenchmarkRetainedHeap does not exercise at all — and was the largest one in a
// live proxy before the schemas were interned: measured at 84KB per event, 4.1x the JSON
// text, because they were held as map[string]any.
//
// Two sub-benchmarks, because a single figure cannot show what interning did:
//
//   - shared: every turn re-sends the SAME manifest, which is what a real client does.
//     One copy per session survives.
//   - distinct: every turn's manifest differs by one nonce field, so the table can never
//     match. Standing next to `shared` it prices the duplication that was being paid.
//
// Measured when this was written: shared 2.78MB/session, distinct 7.53MB — the tool term
// collapsing from ~5.3MB to ~0.6MB.
//
// What `distinct` is NOT is a reproduction of the old cost. It prices duplication only,
// and the field it replaced was a map[string]any, which cost 4.1x its JSON text to hold
// on top of being duplicated. The real change is therefore larger than the ratio here,
// and the map figure is not reproducible in this tree by design — the type is gone.
//
// The interleaved tunnel-open in the loop below is load bearing, not incidental colour.
// With it present and InternEvent's contentless-event guard removed, `shared` measures
// 31.33MB/session against 2.79MB — 11.2x, because every turn's table was being cleared
// before the next turn could match it. Any change to the rolling table should be measured
// on THIS shape rather than on a clean run of turns, which is not what live traffic is.
//
// Reported rather than asserted, for the reasons on BenchmarkRetainedHeap:
//
//	go test ./session/ -bench RetainedHeapWithTools -run '^$' -benchtime 1x
func BenchmarkRetainedHeapWithTools(b *testing.B) {
	for _, tc := range []struct {
		name  string
		nonce func(turn int) int
	}{
		{"shared", func(int) int { return 0 }},
		{"distinct", func(turn int) int { return turn }},
	} {
		b.Run(tc.name, func(b *testing.B) {
			for i := 0; i < b.N; i++ {
				runtime.GC()
				var before runtime.MemStats
				runtime.ReadMemStats(&before)

				s := New(0, 0, 100)
				for turn := 1; turn <= benchTurns; turn++ {
					// A tunnel-open between the turns, as the live traffic has: every
					// bridged HTTPS request records one, and it lands between an inference
					// request and its response. It carries nothing to intern, so it is
					// also the shape that used to reset the table — see InternEvent.
					s.Append("s1", pipeline.SessionEvent{Tunnel: true, Host: "gateway:443"})
					s.Append("s1", pipeline.SessionEvent{
						Inference: &pipeline.InferenceExtension{
							Messages: benchConversation(turn),
							Tools:    benchToolManifest(tc.nonce(turn)),
						},
					})
				}

				runtime.GC()
				var after runtime.MemStats
				runtime.ReadMemStats(&after)
				delta := int64(after.HeapAlloc) - int64(before.HeapAlloc)
				b.ReportMetric(float64(delta)/1048576, "MB/session")
				b.ReportMetric(float64(benchTurns), "turns")

				s.Close()
				runtime.KeepAlive(s)
			}
		})
	}
}

// benchToolResults builds the tool results one turn re-sends: every earlier result plus
// one new one, as a parser rebuilds them. The content is sized like a real result (a
// whole file); the id is the ~20 bytes a "toolu_…"/"call_…" identifier actually is, which
// is the point — it sits under InternMinLen and is interned by internID, not intern.
func benchToolResults(turn int) []pipeline.InferenceToolResult {
	out := make([]pipeline.InferenceToolResult, 0, turn)
	for i := 0; i < turn; i++ {
		out = append(out, pipeline.InferenceToolResult{
			ToolUseID: fmt.Sprintf("toolu_%03d", i),
			Content:   fmt.Sprintf("result %03d: %s", i, strings.Repeat("output text ", 20)),
		})
	}
	return out
}

// BenchmarkRetainedHeapWithToolResults reports what the tool-result headers cost a
// session — the term neither benchmark above exercises, because it is not the content
// (already interned by threshold) but the PER-EVENT headers: the slice of
// {ToolUseID, IsError} structs plus the id strings, which repeat across events while
// being too short for intern's floor. internID removed the string half; the struct
// headers remain, and this is the number that prices them.
//
// Measured when this was written, 300 turns of a session whose tool results also carry
// interned ~240-byte contents: 4.99 MB/session with the ids duplicated (internID
// disabled), 4.32 MB after — 0.67 MB of pure id duplication, against ~2 MB of struct
// headers that no string sharing can remove. The id term grows with the square of the
// turn count for the same reason the conversation's duplication did; on a session of
// chatty tool use it is the difference between the transcript and the transcript plus
// a copy of every id per turn.
//
// Reported rather than asserted, for the reasons on BenchmarkRetainedHeap:
//
//	go test ./session/ -bench RetainedHeapWithToolResults -run '^$' -benchtime 1x
func BenchmarkRetainedHeapWithToolResults(b *testing.B) {
	for i := 0; i < b.N; i++ {
		runtime.GC()
		var before runtime.MemStats
		runtime.ReadMemStats(&before)

		s := New(0, 0, 100)
		for turn := 1; turn <= benchTurns; turn++ {
			// A tunnel-open between the turns, for the reason every fixture here
			// carries one — see BenchmarkRetainedHeapWithTools.
			s.Append("s1", pipeline.SessionEvent{Tunnel: true, Host: "gateway:443"})
			s.Append("s1", pipeline.SessionEvent{
				Inference: &pipeline.InferenceExtension{
					Messages:    benchConversation(turn),
					ToolResults: benchToolResults(turn),
				},
			})
		}

		runtime.GC()
		var after runtime.MemStats
		runtime.ReadMemStats(&after)
		delta := int64(after.HeapAlloc) - int64(before.HeapAlloc)
		b.ReportMetric(float64(delta)/1048576, "MB/session")
		b.ReportMetric(float64(benchTurns), "turns")

		s.Close()
		runtime.KeepAlive(s)
	}
}

// BenchmarkRetainedHeap reports the heap a finished session holds, which is the number
// this package's interning exists to move.
//
// Committed because the figure is otherwise unreproducible after any change to how
// content is stored — and the review of that change found exactly such a regression
// hiding behind a plausible number: keying the intern table on the duplicate rather than
// the canonical string, which reads correctly and silently retains a second copy of the
// conversation.
//
// Reported rather than asserted. A threshold either flakes or is loose enough to miss
// the regressions that matter, so the deterministic properties are pinned by tests
// (TestIntern_TableKeysAreTheStringsTheEventsReference,
// TestAppend_SharesRepeatedMessageContent) and this reports the consequence:
//
//	go test ./session/ -bench RetainedHeap -run '^$' -benchtime 1x
func BenchmarkRetainedHeap(b *testing.B) {
	for i := 0; i < b.N; i++ {
		runtime.GC()
		var before runtime.MemStats
		runtime.ReadMemStats(&before)

		s := New(0, 0, 100)
		for turn := 1; turn <= benchTurns; turn++ {
			s.Append("s1", pipeline.SessionEvent{
				Inference: &pipeline.InferenceExtension{Messages: benchConversation(turn)},
			})
		}

		runtime.GC()
		var after runtime.MemStats
		runtime.ReadMemStats(&after)
		// Signed: HeapAlloc is uint64, and if a GC nets the heap below the baseline the
		// unsigned difference wraps to ~1.8e19 and reports ~1.7e13 MB/session. A small
		// negative is the honest answer and reads as one.
		delta := int64(after.HeapAlloc) - int64(before.HeapAlloc)
		b.ReportMetric(float64(delta)/1048576, "MB/session")
		b.ReportMetric(float64(benchTurns), "turns")

		s.Close()
		runtime.KeepAlive(s)
	}
}

// BenchmarkListSessions_Title prices what /v1/sessions pays for the Title field, and its whole
// point now is that EVERY CASE IS THE SAME PRICE. The title is folded by Store.Append into
// entry.Title, so ListSessions reads a field; the content shapes below cannot move it.
//
// It used to measure a reverse walk over the retained events, under s.mu.RLock() whose writer side
// is Append on the proxy request path — named 280ns, user-text 228µs, no-title 13µs, demoted 714µs
// at benchTurns=300 and maxEvents=100, a ~2500x spread driven entirely by attacker-supplied content.
// A review then measured 38.1s of lock hold on a 190KB message of nested <system-reminder> opens,
// which is what moved the fold to the writer. Now: ~165ns across all five cases.
//
// KEPT, RATHER THAN DELETED WITH THE WALK IT PRICED, because a flat row of five figures is the
// assertion — it is the cheapest available evidence that no content shape reaches the read path any
// more. If any case here diverges from the others again, the walk is back.
//
// The cases are still named for the shapes they exercise, which now bear on Append rather than on
// this benchmark: named (a /rename in the newest event, rank 0), user-text (rank 2, unbeatable so
// nothing terminates early), no-title (assistant-only, rejected on the role comparison), demoted
// (quickRank guesses rank 1 and titleFrom demotes every one — the screen missing 100% of the time),
// reminder-only (the shape that drove the old quadratic retry cascade).
//
// Reported rather than asserted: it is a per-poll cost against agentop's two-second refresh, and
// five figures agreeing on one machine is the useful reading, not an absolute number.
//
//	go test ./session/ -bench ListSessions_Title -run '^$'
func BenchmarkListSessions_Title(b *testing.B) {
	for _, tc := range []struct {
		name string
		role string
		last string // content of one extra message on the newest event
		// body, when set, replaces every message's content — the demoted case needs the miss on
		// ALL of them, not just on one appended message.
		body string
	}{
		{name: "named", role: "user", last: renamePrefix + "<command-args>a name</command-args>"},
		{name: "user-text", role: "user"},
		{name: "no-title", role: "assistant"},
		// Unterminated: quickRank sees the opening tag (rank 1), titleFrom finds no closing tag
		// and settles at rank 2. Every message pays a full titleFrom, which is the screen missing
		// 100% of the time.
		{name: "demoted", role: "user", body: "what does <user_query> mean in " + strings.Repeat("this code ", 45)},
		// REMINDER-BEARING, because without it this benchmark never executed stripReminders at
		// all — the function the second commit exists for — and "parity with baseline" was
		// measured on a fixture with zero reminder content. That blind spot hid a quadratic retry
		// cascade: every message here settles to rankNone, which is the shape that drove
		// titleCandidate's old retry loop to 7.20ms at 800 messages/event.
		{name: "reminder-only", role: "user", last: "<system-reminder>context, no prompt</system-reminder>"},
	} {
		b.Run(tc.name, func(b *testing.B) {
			s := New(0, 100, 0)
			for turn := 1; turn <= benchTurns; turn++ {
				msgs := benchConversation(turn)
				if tc.role != "user" {
					for i := range msgs {
						msgs[i].Role = tc.role
					}
				}
				if tc.body != "" {
					for i := range msgs {
						msgs[i].Content = tc.body
					}
				}
				if tc.last != "" && turn == benchTurns {
					msgs = append(msgs, pipeline.InferenceMessage{Role: "user", Content: tc.last})
				}
				s.Append("bench", pipeline.SessionEvent{
					Phase:     pipeline.SessionResponse,
					Inference: &pipeline.InferenceExtension{Messages: msgs},
				})
			}
			b.ReportAllocs()
			b.ResetTimer()
			// EVERY CASE ABOVE IS EXPECTED TO MEASURE THE SAME THING, and that is the assertion
			// this benchmark makes: the fixture shapes bear on Append, not on the loop below.
			// Store.Append folds the title into entry.Title, so ListSessions reads a field and no
			// content shape can reach it. Five figures agreeing is the evidence; if one diverges
			// from the others, the reverse walk that used to cost 280ns–714µs here is back.
			for i := 0; i < b.N; i++ {
				_ = s.ListSessions()
			}
		})
	}
}

// BenchmarkSessionTitle_ReminderFanout measures the axis BenchmarkListSessions_Title does not:
// MESSAGES PER EVENT, all of them reminder-only, which is the shape that made titleCandidate's
// old retry loop quadratic. That loop re-scanned the whole Messages slice each time titleFrom
// refused a pick, so an event where every message is refused cost O(n²) — 31.9µs at n=50 rising
// to 7.20ms at n=800, a clean 4x per doubling and ~1088x the same event with no demotions, all
// under the store's read lock whose writer side is Append on the request path.
//
// The single reverse scan that replaced it is linear: ~2.0x per doubling, 3.5/14/57µs at
// n=50/200/800.
//
// A SEPARATE BENCHMARK rather than another case in the table above, because the table varies
// turns (events) and this has to vary messages within ONE event — the two axes multiply, and the
// quadratic one was invisible while only the first was measured.
//
// CALLS titleCandidate, which is the function Append actually runs per event — so this measures the
// production walk and not a test-only wrapper around it. (It used to call a per-session picker that
// looped over events; that has been deleted, and nothing is lost here, because the axis this varies
// is messages WITHIN one event.)
//
//	go test ./session/ -bench SessionTitle_ReminderFanout -run '^$'
func BenchmarkSessionTitle_ReminderFanout(b *testing.B) {
	for _, n := range []int{50, 200, 800} {
		b.Run(fmt.Sprintf("msgs=%d", n), func(b *testing.B) {
			msgs := make([]pipeline.InferenceMessage, 0, n)
			for i := 0; i < n; i++ {
				msgs = append(msgs, pipeline.InferenceMessage{
					Role:    "user",
					Content: "<system-reminder>" + strings.Repeat("x", 40) + "</system-reminder>",
				})
			}
			event := pipeline.SessionEvent{Inference: &pipeline.InferenceExtension{Messages: msgs}}
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				_, _ = titleCandidate(&event)
			}
		})
	}
}
