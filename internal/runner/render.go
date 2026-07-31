package runner

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"sync"

	"github.com/steadyspacecorp/openroutines/internal/scrub"
)

// renderer turns opencode's --format json event stream into the bounded run
// log (design decision "Run output is rendered, bounded, and leveled"): the
// routine's own text prints in full, each tool call becomes a summary line,
// failed tools add a bounded diagnostic tail, and anything unrecognized --
// a schema the framework doesn't know, a plain-text line -- passes through
// truncated rather than failing the run. Scrubbing happens before truncation, always:
// a truncation boundary must never split a secret past the exact-value
// matcher.
type renderer struct {
	dst      io.Writer
	secrets  map[string]string // name -> value, same map the scrub writer takes
	buf      bytes.Buffer
	dropping bool // mid-line beyond maxEventBytes: discard to the next newline
}

const (
	// maxEventBytes bounds one buffered event line. A tool event carries its
	// whole output in a single line of JSON, so the buffer must hold real
	// tool output -- but an extreme one is suppressed with a notice rather
	// than ballooning memory.
	maxEventBytes = 8 << 20
	// toolOutputBytes is how much of a tool call's output survives into the
	// log: the tail, where results and errors land.
	toolOutputBytes = 2048
	// passthroughBytes bounds a line the renderer does not recognize.
	passthroughBytes = 2048
)

func newRenderer(dst io.Writer, secrets map[string]string) *renderer {
	return &renderer{dst: dst, secrets: secrets}
}

// prefixWriter attributes run output to its routine: runs execute
// concurrently and share one stdout, so an unattributed line could belong to
// any of them. Lines are held until complete and each is written in a single
// Write, so two runs' lines interleave whole, never mid-line. It wraps only
// the log sink -- the tail buffer sits beside it, because failure
// classification and the failure tail read raw output.
type prefixWriter struct {
	dst    io.Writer
	prefix []byte
	buf    bytes.Buffer
}

func newPrefixWriter(dst io.Writer, name string) *prefixWriter {
	return &prefixWriter{dst: dst, prefix: []byte(name + " | ")}
}

func (w *prefixWriter) Write(p []byte) (int, error) {
	w.buf.Write(p)
	for {
		i := bytes.IndexByte(w.buf.Bytes(), '\n')
		if i < 0 {
			// A stream that never sends a newline must not grow the buffer
			// without bound; past the same cap the renderer applies, the
			// partial line goes out as one.
			if w.buf.Len() > maxEventBytes {
				w.Flush()
			}
			return len(p), nil
		}
		line := w.buf.Next(i + 1)
		out := make([]byte, 0, len(w.prefix)+len(line))
		out = append(out, w.prefix...)
		out = append(out, line...)
		if _, err := w.dst.Write(out); err != nil {
			return len(p), err
		}
	}
}

// Flush writes the partial line the stream ended on, newline-terminated.
func (w *prefixWriter) Flush() {
	if w.buf.Len() == 0 {
		return
	}
	out := make([]byte, 0, len(w.prefix)+w.buf.Len()+1)
	out = append(out, w.prefix...)
	out = append(out, w.buf.Bytes()...)
	out = append(out, '\n')
	w.buf.Reset()
	_, _ = w.dst.Write(out)
}

// syncWriter serializes the two stream renderers' writes: os/exec drains a
// run's stdout and stderr on separate goroutines, and both land on the same
// destination -- the prefix writer's line buffer and the tail behind it.
type syncWriter struct {
	mu sync.Mutex
	w  io.Writer
}

func (s *syncWriter) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.w.Write(p)
}

// event is the slice of an opencode run event the renderer reads.
type event struct {
	Type string `json:"type"`
	Part struct {
		Tool  string `json:"tool"`
		Text  string `json:"text"`
		State struct {
			Status   string `json:"status"`
			Title    string `json:"title"`
			Output   string `json:"output"`
			Error    string `json:"error"`
			Metadata struct {
				Exit *int64 `json:"exit"`
			} `json:"metadata"`
		} `json:"state"`
	} `json:"part"`
	Error json.RawMessage `json:"error"`
}

func (r *renderer) Write(p []byte) (int, error) {
	r.buf.Write(p)
	for {
		i := bytes.IndexByte(r.buf.Bytes(), '\n')
		if i < 0 {
			if r.buf.Len() > maxEventBytes {
				if !r.dropping {
					r.dropping = true
					_, _ = fmt.Fprintf(r.dst, "[run output: an event over %dMB was suppressed]\n", maxEventBytes>>20)
				}
				r.buf.Reset()
			}
			return len(p), nil
		}
		line := string(r.buf.Next(i + 1))
		if r.dropping {
			r.dropping = false // the tail of the oversized line
			continue
		}
		r.render(line)
	}
}

// Flush renders whatever partial line the stream ended on.
func (r *renderer) Flush() {
	if r.dropping || r.buf.Len() == 0 {
		return
	}
	line := r.buf.String()
	r.buf.Reset()
	r.render(line)
}

func (r *renderer) render(line string) {
	line = strings.TrimRight(line, "\r\n")
	if strings.TrimSpace(line) == "" {
		return
	}
	var ev event
	if json.Unmarshal([]byte(line), &ev) != nil || ev.Type == "" {
		r.emit(firstBytes(scrub.Redact(line, r.secrets), passthroughBytes))
		return
	}
	switch ev.Type {
	case "step_start", "step_finish":
		// Transcript structure, not content; tokens are captured from the
		// session record, not the stream.
	case "text":
		// The routine's own output: model-generated, token-bounded, printed
		// whole -- this is the part of a run a person reads.
		r.emit(scrub.Redact(ev.Part.Text, r.secrets))
	case "tool_use":
		r.renderTool(&ev)
	case "error":
		// Keep the payload: failure classification (the provider-auth hint)
		// matches on this text, and so does the operator reading the log.
		r.emit(firstBytes(scrub.Redact("[error] "+string(ev.Error), r.secrets), passthroughBytes))
	default:
		r.emit(firstBytes(scrub.Redact(line, r.secrets), passthroughBytes))
	}
}

// renderTool prints one completed tool call. Success is one summary line with
// the amount of output suppressed; a failure also keeps the scrubbed output
// tail, because that is the immediate diagnostic even when the model recovers.
func (r *renderer) renderTool(ev *event) {
	st := &ev.Part.State
	line := fmt.Sprintf("[tool %s] %s", ev.Part.Tool, firstBytes(scrub.Redact(st.Title, r.secrets), 256))
	var notes []string
	if st.Status != "" && st.Status != "completed" {
		notes = append(notes, st.Status)
	}
	if st.Metadata.Exit != nil {
		notes = append(notes, fmt.Sprintf("exit %d", *st.Metadata.Exit))
	}
	output := st.Output
	if st.Error != "" {
		output = st.Error
	}
	output = scrub.Redact(output, r.secrets)
	failed := st.Error != "" || (st.Metadata.Exit != nil && *st.Metadata.Exit != 0) || (st.Status != "" && st.Status != "completed")
	if strings.TrimSpace(output) != "" {
		if failed {
			if len(output) > toolOutputBytes {
				notes = append(notes, fmt.Sprintf("%s output, last %s shown", byteSize(len(output)), byteSize(toolOutputBytes)))
				output = lastBytes(output, toolOutputBytes)
			}
		} else {
			notes = append(notes, fmt.Sprintf("%s output suppressed", byteSize(len(output))))
			output = ""
		}
	}
	if len(notes) > 0 {
		line += " (" + strings.Join(notes, ", ") + ")"
	}
	r.emit(line)
	if strings.TrimSpace(output) != "" {
		r.emit(strings.TrimRight(output, "\n"))
	}
}

// emit writes one already-scrubbed, already-bounded chunk as its own line.
func (r *renderer) emit(s string) {
	_, _ = fmt.Fprintln(r.dst, s)
}

// firstBytes keeps the first max bytes of an already-scrubbed string.
func firstBytes(s string, limit int) string {
	if len(s) <= limit {
		return s
	}
	return s[:limit] + fmt.Sprintf(" [... %s truncated]", byteSize(len(s)-limit))
}

// lastBytes keeps the last max bytes of an already-scrubbed string.
func lastBytes(s string, limit int) string {
	if len(s) <= limit {
		return s
	}
	return s[len(s)-limit:]
}

func byteSize(n int) string {
	switch {
	case n >= 1<<20:
		return fmt.Sprintf("%.1fMB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.1fKB", float64(n)/(1<<10))
	}
	return fmt.Sprintf("%dB", n)
}
