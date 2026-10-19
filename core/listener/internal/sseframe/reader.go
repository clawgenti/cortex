// Package sseframe reads complete Server-Sent Event frames off a
// streaming HTTP response body. The reader emits one slice per
// "data:"-bearing event, with all data lines of that event
// concatenated and delivered together — exactly the shape MCP's
// Streamable HTTP transport, A2A's message/stream, and OpenAI-
// compatible chat-completions streaming use to carry one JSON-RPC
// message (or one chunk) per event.
//
// This is a deliberately narrow subset of the SSE spec
// (https://html.spec.whatwg.org/multipage/server-sent-events.html):
// ReadFrame surfaces only the data payload of each event; comment lines,
// "event:", "id:" and "retry:" fields are parsed past. ReadBlock also
// returns each block's exact wire bytes, so a proxy can forward the
// stream unchanged while plugins read the payload. The framing this code
// cares about is:
//
//	data: { ... json-rpc message ... }\n
//	\n
//	data: { ... another message ... }\n
//	\n
//
// Multi-line data within a single event is folded with "\n"
// separators per the spec; an empty line ends the event. CR/LF and
// CRLF terminators are accepted.
package sseframe

import (
	"bufio"
	"bytes"
	"errors"
	"io"
)

// ErrFrameTooLarge is returned by Reader.ReadFrame when a single SSE
// event's accumulated data exceeds the configured per-frame cap.
// Streaming responses bound memory to one frame at a time, so this
// signals an upstream that produced a single oversized JSON-RPC
// message — the proxy gives up rather than buffering it.
var ErrFrameTooLarge = errors.New("sseframe: frame exceeds per-frame size cap")

// utf8BOM is U+FEFF encoded in UTF-8, which is how it arrives on the wire.
var utf8BOM = []byte{0xEF, 0xBB, 0xBF}

// Reader scans an io.Reader for complete SSE events and returns the
// concatenated data lines of each. Construct with NewReader; call
// ReadFrame repeatedly until io.EOF.
type Reader struct {
	br      *bufio.Reader
	maxSize int
	// scratch holds the in-progress event's accumulated data lines.
	// Reused across ReadFrame calls to avoid per-frame allocation in
	// the common steady-state case.
	scratch []byte
	// raw holds the in-progress block's exact wire bytes, terminators and a
	// leading BOM included. Reused across calls like scratch.
	raw []byte
	// bomChecked records that the one-time byte-order-mark check has run. The BOM is a
	// property of the STREAM's first bytes, not of each frame, so this must not be reset
	// between frames — see ReadFrame.
	bomChecked bool
	// lastEvent holds the "event:" field value of the block most
	// recently returned (empty if it named none). Reused across calls;
	// valid until the next read. No proxy needs it — they forward
	// Block.Raw — but it is how the BOM tests observe the first event's type.
	lastEvent []byte
}

// NewReader wraps r with the given per-frame size cap (bytes).
// A non-positive maxSize falls back to the package default
// (DefaultMaxFrameSize).
func NewReader(r io.Reader, maxSize int) *Reader {
	if maxSize <= 0 {
		maxSize = DefaultMaxFrameSize
	}
	return &Reader{
		br:      bufio.NewReader(r),
		maxSize: maxSize,
	}
}

// DefaultMaxFrameSize bounds a single SSE event's data payload at 1 MiB when
// NewReader is given a non-positive maxSize. It is a defensive fallback only:
// every caller in this repo passes an explicit cap, and those caps no longer
// agree with each other or with this value — the forward proxy passes 10MB and
// the reverse proxy 1MB, each from its own listener's maxBodySize. So this
// constant no longer describes the per-message ceiling any real stream runs
// under; read the calling listener's maxBodySize for that.
const DefaultMaxFrameSize = 1 << 20

// Block is one SSE block as it crossed the wire: every line up to and
// including the blank line that ends it, or up to EOF for a final block
// the upstream never terminated.
type Block struct {
	// Raw is the block's exact upstream bytes — field lines, comments,
	// CR/LF/CRLF terminators and, on the first block, a leading BOM.
	// Writing every block's Raw in order reproduces the upstream body.
	Raw []byte
	// Data is the block's data lines folded with "\n" — what ReadFrame
	// returns for it.
	Data []byte
	// HasData reports whether the block carried a data line. Blocks of
	// only comments, event:, id: or retry: lines have none, and plugins
	// are not shown them.
	HasData bool
}

// ReadBlock reads the next SSE block. It returns (block, nil) for every
// block with at least one byte, (Block{}, io.EOF) once the stream is
// exhausted, and (Block{}, err) on ErrFrameTooLarge or a read error — no
// partial block, so a relay never forwards half of one. Raw and Data are
// owned by the Reader and valid until the next ReadBlock or ReadFrame.
func (r *Reader) ReadBlock() (Block, error) {
	r.scratch = r.scratch[:0]
	r.raw = r.raw[:0]
	r.lastEvent = r.lastEvent[:0]
	hasData := false
	// nonData counts the bytes of this block's non-data lines. They never
	// reach scratch, so the data cap does not bound them; this does, at the
	// same size. Raw itself is not capped: its data lines are already bounded
	// by the data cap, and capping Raw would refuse an event of many short
	// data lines that the data cap admits ("data: a\n" is 8 bytes for 1).
	nonData := 0

	// ONE LEADING BYTE-ORDER MARK IS REMOVED, and the spec requires it: "If the stream begins
	// with a U+FEFF BYTE ORDER MARK character, then remove it."
	//
	// WHAT IT COSTS DEPENDS ON THE SHAPE, and on the shape gateways actually send it costs the first
	// frame its TYPE. Without this, the first LINE reads as a field whose name begins with those
	// three bytes, matching neither "data" nor "event", and is skipped:
	//
	//	event:-first (Anthropic, LiteLLM)  the "event:" line is skipped, so the frame and its
	//	                                   payload survive and LastEvent comes back empty
	//	data:-only                         the "data:" line is skipped, so the whole FIRST EVENT
	//	                                   goes with it
	//
	// The first row is the one that matters, because it is what this repo's traffic and every other
	// SSE fixture in it look like: plugins key the first event by its type. The BOM itself is kept
	// in Block.Raw, so a relay still forwards the stream as sent.
	//
	// HERE RATHER THAN IN THE PARSERS, because this reader is what every PER-FRAME path uses —
	// ext_proc's buffered re-parse and both proxies' streaming paths. inferenceparser's
	// normalizeSSE strips a BOM too, but only for the two whole-body parsers that never touch
	// this reader, so a fix there covers none of the streaming traffic.
	//
	// ONCE, at the stream's start, which is why the flag is on the Reader and not a local: a BOM
	// arriving mid-stream is an upstream error this does not paper over, and scanning for one
	// would corrupt any payload legitimately containing those bytes.
	if !r.bomChecked {
		r.bomChecked = true
		if first, err := r.br.Peek(len(utf8BOM)); err == nil && bytes.Equal(first, utf8BOM) {
			_, _ = r.br.Discard(len(utf8BOM))
			// Stripped for parsing, kept for the wire: the client is owed
			// the stream as the upstream sent it.
			r.raw = append(r.raw, utf8BOM...)
		}
	}

	for {
		line, err := r.readLine()
		if err == io.EOF {
			// Stream ended. A final block the upstream never terminated is
			// still a block: its data is dispatched — per spec "if the user
			// agent has reached the end of the file, then dispatch the
			// event" — and its bytes forwarded without a blank line the
			// upstream did not send.
			if len(r.raw) > 0 {
				return r.block(hasData), nil
			}
			return Block{}, io.EOF
		}
		if err != nil {
			return Block{}, err
		}

		// Blank line — the block ends here, data or not, so a heartbeat
		// reaches the client now rather than with the next event.
		if len(line) == 0 {
			return r.block(hasData), nil
		}

		// Comment line — silently skipped per spec ("If the line
		// starts with a U+003A COLON character, ignore the line").
		if field, _ := splitField(line); field != "data" {
			nonData += len(line)
			if nonData > r.maxSize {
				return Block{}, ErrFrameTooLarge
			}
		}
		if line[0] == ':' {
			continue
		}

		// Field-bearing line. Split at the first colon; everything
		// after it (with one optional leading space stripped) is the
		// value. Lines without a colon name a field with an empty
		// value, which we don't care about either way.
		field, value := splitField(line)
		if field == "event" {
			// Capture the event type (see LastEvent). Copied because value points into
			// readLine's buffer, which is reused on the next line read.
			r.lastEvent = append(r.lastEvent[:0], value...)
			continue
		}
		if field != "data" {
			// We deliberately ignore "id", "retry", and any unknown
			// field — the consumer only needs the data payload (and the
			// event type captured above).
			continue
		}

		// Append "\n" before the next data line per spec ("If the
		// data buffer's last character is a U+000A LINE FEED (LF)
		// character, then remove the last character from the data
		// buffer." — equivalently, separator between data lines is
		// LF, and trailing LF is stripped at dispatch).
		if hasData {
			if err := r.appendByte('\n'); err != nil {
				return Block{}, err
			}
		}
		hasData = true
		if err := r.appendBytes(value); err != nil {
			return Block{}, err
		}
	}
}

func (r *Reader) block(hasData bool) Block {
	return Block{Raw: r.raw, Data: r.scratch, HasData: hasData}
}

// ReadFrame reads from the underlying reader until a complete SSE
// event arrives (i.e. a blank line terminator after one or more
// "data:" lines), then returns the concatenated data payload.
// Empty events (no "data:" lines, or only comment/event-type lines)
// are skipped silently — the returned slice is always non-empty
// unless the stream ends.
//
// At end-of-stream the function returns (nil, io.EOF). If the stream
// ends after a partial event with data already accumulated, that
// trailing event is delivered before EOF (per the spec, an SSE
// stream may end without a final blank-line terminator).
//
// The returned slice is owned by the Reader and reused on the next
// call; callers that need to retain bytes across ReadFrame calls
// must copy.
func (r *Reader) ReadFrame() ([]byte, error) {
	for {
		b, err := r.ReadBlock()
		if err != nil {
			return nil, err
		}
		if b.HasData {
			return b.Data, nil
		}
	}
}

// LastEvent returns the "event:" field value of the frame most recently
// returned by ReadFrame, or an empty slice if that frame named no event
// type. The returned slice is owned by the Reader and reused on the next
// ReadFrame call; copy to retain. Proxies do not use it — they forward
// Block.Raw unchanged.
func (r *Reader) LastEvent() []byte { return r.lastEvent }

// readLine reads one logical SSE line off the underlying buffered
// reader. Line terminators per the SSE spec are LF, CR, or CRLF —
// any of the three ends a line. The returned slice excludes the
// terminator and is valid only until the next call. EOF without a
// final newline is reported as io.EOF only when the line is empty;
// otherwise the unterminated tail is returned. Every byte it consumes,
// terminators included, is appended to r.raw.
//
// Per-line bytes are bounded by r.maxSize so a pathological non-data
// line (huge comment, malformed unknown field) cannot grow memory
// past the per-frame cap.
func (r *Reader) readLine() ([]byte, error) {
	var line []byte
	for {
		b, err := r.br.ReadByte()
		if err != nil {
			if err == io.EOF && len(line) > 0 {
				return line, nil
			}
			return nil, err
		}
		r.raw = append(r.raw, b)
		switch b {
		case '\n':
			return line, nil
		case '\r':
			// Coalesce CRLF as a single terminator. If the next byte
			// isn't LF, leave it for the next ReadByte; a bare CR is
			// itself a valid line terminator.
			if next, perr := r.br.Peek(1); perr == nil && len(next) == 1 && next[0] == '\n' {
				_, _ = r.br.ReadByte()
				r.raw = append(r.raw, '\n')
			}
			return line, nil
		default:
			if len(line)+1 > r.maxSize {
				return nil, ErrFrameTooLarge
			}
			line = append(line, b)
		}
	}
}

// appendByte / appendBytes grow the scratch buffer while enforcing
// the per-frame cap. ErrFrameTooLarge fires the moment we'd exceed
// the cap, before any further reading — bounding memory to maxSize
// regardless of what the upstream sends.
func (r *Reader) appendByte(b byte) error {
	if len(r.scratch)+1 > r.maxSize {
		return ErrFrameTooLarge
	}
	r.scratch = append(r.scratch, b)
	return nil
}

func (r *Reader) appendBytes(b []byte) error {
	if len(r.scratch)+len(b) > r.maxSize {
		return ErrFrameTooLarge
	}
	r.scratch = append(r.scratch, b...)
	return nil
}

// splitField parses an SSE field line "name:value" or "name:". A
// single optional leading space after the colon is stripped per spec.
// Lines with no colon are treated as a field name with empty value.
func splitField(line []byte) (string, []byte) {
	idx := bytes.IndexByte(line, ':')
	if idx < 0 {
		return string(line), nil
	}
	name := string(line[:idx])
	value := line[idx+1:]
	if len(value) > 0 && value[0] == ' ' {
		value = value[1:]
	}
	return name, value
}
