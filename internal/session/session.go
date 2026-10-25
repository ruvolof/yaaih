// Package session persists a harness run's transcript to a JSONL file, one
// object per line, append-only, flushed after every message. A prior session
// can be resumed via session.Load + OpenForAppend so crashes don't lose
// history.
//
// File layout (UTF-8, no BOM):
//
//	{"type":"header","version":1,"id":"...","started":"...","agent_path":"...","model":"...","provider":"..."}
//	{"type":"message","seq":1,"role":"system","content":"You are ..."}
//	{"type":"message","seq":2,"role":"user","content":"deploy staging"}
//	{"type":"message","seq":3,"role":"assistant","tool_calls":[...]}
//	{"type":"message","seq":4,"role":"tool","tool_call_id":"call_abc","name":"shell_exec","content":"..."}
//	{"type":"footer","ended":"...","reason":"clean"}
//
// The header and footer are optional on read: a file without a footer is a
// partial session (typically a crash) and is loaded the same way.
package session

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/ruvolof/yaaih/internal/llm"
	"github.com/ruvolof/yaaih/internal/logging"
)

var log = logging.For("session")

// formatVersion is written into each header and lets future readers detect an
// incompatible on-disk shape.
const formatVersion = 1

// ID identifies a session: "YYYYMMDD-HHMMSS-<4 random chars>".
type ID string

// Header is the first line of a session file.
type Header struct {
	Type      string `json:"type"` // always "header"
	Version   int    `json:"version"`
	ID        ID     `json:"id"`
	Started   string `json:"started"`
	AgentPath string `json:"agent_path"`
	Model     string `json:"model"`
	Provider  string `json:"provider"`
	// Parent and Task are set on a session spawned from another one: the
	// session it was briefed from and the task it was started on.
	Parent ID     `json:"parent,omitempty"`
	Task   string `json:"task,omitempty"`
}

// Usage is the token/cost usage of one LLM call, persisted on the assistant
// entry it produced. Summing it across a session restores that session's
// cumulative usage on resume.
type Usage struct {
	InputTokens   int     `json:"input_tokens"`
	OutputTokens  int     `json:"output_tokens"`
	Cost          float64 `json:"cost,omitempty"`
	CostAvailable bool    `json:"cost_available,omitempty"`
}

// Entry is one transcript message line. ToolCalls reuses the llm type so the
// agent can map to/from its conversation history without a translation layer.
type Entry struct {
	Type       string         `json:"type"` // always "message"
	Seq        int            `json:"seq"`
	Role       string         `json:"role"`
	Content    string         `json:"content,omitempty"`
	ToolCalls  []llm.ToolCall `json:"tool_calls,omitempty"`
	ToolCallID string         `json:"tool_call_id,omitempty"`
	Name       string         `json:"name,omitempty"`  // tool name for role=tool
	Usage      *Usage         `json:"usage,omitempty"` // set on assistant entries that carried usage
}

// Compaction records that a span of the conversation was replaced by a summary
// of it. From and Through are positions in the conversation as it stood when
// the line was written — a half-open range over the messages Load has rebuilt
// by the time it reaches this line, which is the same list the agent held.
// Recording positions rather than sequence numbers is what lets a compaction
// sit on top of an earlier one without either having to know about the other.
type Compaction struct {
	Type    string `json:"type"` // always "compaction"
	Seq     int    `json:"seq"`
	From    int    `json:"from_index"`
	Through int    `json:"through_index"`
	Role    string `json:"role"`
	Content string `json:"content"`
}

// SystemPrompt records that the conversation carried on under a different
// system prompt from the one the transcript opens with: a skill was loaded, or
// the session was resumed after agent.md or its skills were edited. Every
// request after this line was sent with Content in place of the earlier prompt,
// and Skills names the on-demand skills Content was composed with.
type SystemPrompt struct {
	Type    string   `json:"type"` // always "system_prompt"
	Seq     int      `json:"seq"`
	Content string   `json:"content"`
	Skills  []string `json:"skills,omitempty"`
}

// footer is the last line of a cleanly-closed session file.
type footer struct {
	Type   string `json:"type"` // always "footer"
	Ended  string `json:"ended"`
	Reason string `json:"reason"` // clean | signal | crash (diagnostic only)
}

// idRE constrains a session id to characters that are safe as a bare
// filename, preventing path traversal from user-supplied names (/save, /resume).
var idRE = regexp.MustCompile(`^[A-Za-z0-9._-]+$`)

// ValidateID reports whether id is usable as a session filename.
func ValidateID(id ID) error {
	s := string(id)
	if s == "" {
		return fmt.Errorf("session: id is empty")
	}
	if !idRE.MatchString(s) {
		return fmt.Errorf("session: id %q may contain only letters, digits, '.', '_', '-'", s)
	}
	return nil
}

// NewID generates a fresh session id.
func NewID() ID {
	const charset = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"
	var b strings.Builder
	for i := 0; i < 4; i++ {
		b.WriteByte(charset[rand.IntN(len(charset))])
	}
	return ID(time.Now().Format("20060102-150405") + "-" + b.String())
}

// Writer appends entries to a single session file. Safe for concurrent use.
type Writer struct {
	mu     sync.Mutex
	f      *os.File
	bw     *bufio.Writer
	id     ID
	closed bool
}

// NewWriter creates <id>.jsonl inside the sessions directory dir, writes the
// header, and returns a ready Writer. dir is created if missing.
func NewWriter(dir string, hdr Header) (*Writer, error) {
	if hdr.ID == "" {
		return nil, fmt.Errorf("session: header is missing an id")
	}
	hdr.Type = "header"
	if hdr.Version == 0 {
		hdr.Version = formatVersion
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("session: mkdir %s: %w", dir, err)
	}
	path := filepath.Join(dir, string(hdr.ID)+".jsonl")
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return nil, fmt.Errorf("session: open %s: %w", path, err)
	}
	w := &Writer{f: f, bw: bufio.NewWriter(f), id: hdr.ID}
	if err := w.writeLine(hdr); err != nil {
		_ = f.Close()
		return nil, err
	}
	return w, nil
}

// OpenForAppend reopens an existing session file in dir for appending without
// writing a new header. Used by the --resume path so a resumed session
// continues the same file rather than starting a duplicate header.
func OpenForAppend(dir string, id ID) (*Writer, error) {
	path := filepath.Join(dir, string(id)+".jsonl")
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return nil, fmt.Errorf("session: reopen %s: %w", path, err)
	}
	return &Writer{f: f, bw: bufio.NewWriter(f), id: id}, nil
}

// ID returns the session id this writer is appending to.
func (w *Writer) ID() ID { return w.id }

// Append writes one message Entry and flushes. Thread-safe.
func (w *Writer) Append(e Entry) error {
	e.Type = "message"
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closed {
		return fmt.Errorf("session: writer closed")
	}
	return w.writeLine(e)
}

// AppendCompaction writes one Compaction line and flushes. Thread-safe.
func (w *Writer) AppendCompaction(c Compaction) error {
	c.Type = "compaction"
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closed {
		return fmt.Errorf("session: writer closed")
	}
	return w.writeLine(c)
}

// AppendSystemPrompt writes one SystemPrompt line and flushes. Thread-safe.
func (w *Writer) AppendSystemPrompt(p SystemPrompt) error {
	p.Type = "system_prompt"
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closed {
		return fmt.Errorf("session: writer closed")
	}
	return w.writeLine(p)
}

// Close writes a footer with the given reason (clean | signal | crash) and
// closes the file. Idempotent.
func (w *Writer) Close(reason string) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closed {
		return nil
	}
	w.closed = true
	ferr := w.writeLine(footer{
		Type:   "footer",
		Ended:  time.Now().UTC().Format(time.RFC3339),
		Reason: reason,
	})
	cerr := w.f.Close()
	if ferr != nil {
		return ferr
	}
	return cerr
}

// Rename renames the active session file to newID and keeps writing to it. The
// session is not finalised — no footer is written — because it continues under
// the new name. The underlying handle is closed and reopened, since Windows
// cannot rename a file that is still open. On failure the writer is left
// usable under its original name where possible.
//
// The id recorded inside the file's header line is not rewritten; the filename
// is the authoritative session id everywhere (List and Load address sessions
// by filename), so the in-file header id simply records where the session
// began.
func (w *Writer) Rename(dir string, newID ID) error {
	if err := ValidateID(newID); err != nil {
		return err
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closed {
		return fmt.Errorf("session: writer closed")
	}
	oldPath := filepath.Join(dir, string(w.id)+".jsonl")
	newPath := filepath.Join(dir, string(newID)+".jsonl")
	if filepath.Clean(oldPath) == filepath.Clean(newPath) {
		return nil // renaming to the same name is a no-op
	}
	if _, err := os.Stat(newPath); err == nil {
		return fmt.Errorf("session: %q already exists", newID)
	}
	if err := w.bw.Flush(); err != nil {
		return fmt.Errorf("session: flush before rename: %w", err)
	}
	if err := w.f.Close(); err != nil {
		return fmt.Errorf("session: close before rename: %w", err)
	}
	if err := os.Rename(oldPath, newPath); err != nil {
		// Reopen the original so the writer stays usable.
		if f, rerr := os.OpenFile(oldPath, os.O_APPEND|os.O_WRONLY, 0o644); rerr == nil {
			w.f, w.bw = f, bufio.NewWriter(f)
		} else {
			w.closed = true
		}
		return fmt.Errorf("session: rename %s -> %s: %w", oldPath, newPath, err)
	}
	f, err := os.OpenFile(newPath, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		w.closed = true
		return fmt.Errorf("session: reopen after rename: %w", err)
	}
	w.f, w.bw, w.id = f, bufio.NewWriter(f), newID
	return nil
}

// writeLine marshals v, writes it plus a newline, and flushes so a crash
// leaves at most the in-flight line unwritten. Caller must hold w.mu.
func (w *Writer) writeLine(v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return fmt.Errorf("session: marshal: %w", err)
	}
	if _, err := w.bw.Write(b); err != nil {
		return fmt.Errorf("session: write: %w", err)
	}
	if err := w.bw.WriteByte('\n'); err != nil {
		return fmt.Errorf("session: write newline: %w", err)
	}
	if err := w.bw.Flush(); err != nil {
		return fmt.Errorf("session: flush: %w", err)
	}
	return nil
}

// Transcript is a session file read back: what a resumed agent continues from.
type Transcript struct {
	Header   Header
	Messages []llm.Message
	Usage    Usage
	// Skills are the on-demand skills the conversation had loaded when the
	// file ends, as the last system prompt line recorded them.
	Skills []string
}

// Load reads a session file and returns its header, the reconstructed
// conversation, and its accumulated usage. See LoadTranscript.
func Load(dir string, id ID) (Header, []llm.Message, Usage, error) {
	transcript, err := LoadTranscript(dir, id)
	return transcript.Header, transcript.Messages, transcript.Usage, err
}

// LoadTranscript reads a session file and reconstructs the conversation it
// records. A missing footer is fine (partial session). A trailing dangling
// assistant message — one whose tool_calls have no matching tool result, which
// happens when a crash lands between the assistant write and the tool result
// write — is elided so the next provider call sees a valid sequence.
func LoadTranscript(dir string, id ID) (Transcript, error) {
	path := filepath.Join(dir, string(id)+".jsonl")
	f, err := os.Open(path)
	if err != nil {
		return Transcript{}, fmt.Errorf("session: open %s: %w", path, err)
	}
	defer f.Close()

	var hdr Header
	var msgs []llm.Message
	var usage Usage
	var loadedSkills []string
	answered := map[string]bool{} // tool_call ids that received a tool result

	// Lines are read whole however long they are: every line was a message held
	// in memory when it was written, and dropping one would leave a tool call
	// unanswered or shift the positions a later compaction line indexes into.
	reader := bufio.NewReaderSize(f, 64*1024)
	lineNo := 0
	for {
		line, readErr := reader.ReadBytes('\n')
		if readErr != nil {
			if !errors.Is(readErr, io.EOF) {
				return Transcript{Header: hdr}, fmt.Errorf("session: read %s: %w", path, readErr)
			}
			if len(line) == 0 {
				break
			}
		}
		lineNo++
		raw := bytes.TrimRight(line, "\r\n")
		if len(raw) == 0 {
			continue
		}
		var probe struct {
			Type string `json:"type"`
		}
		if err := json.Unmarshal(raw, &probe); err != nil {
			log.Warn("session line skipped (unparseable)", "path", path, "line", lineNo, "err", err.Error())
			continue
		}
		switch probe.Type {
		case "header":
			if err := json.Unmarshal(raw, &hdr); err != nil {
				log.Warn("session header skipped", "path", path, "line", lineNo, "err", err.Error())
			}
		case "message":
			var e Entry
			if err := json.Unmarshal(raw, &e); err != nil {
				log.Warn("session message skipped", "path", path, "line", lineNo, "err", err.Error())
				continue
			}
			if e.Role == string(llm.RoleTool) && e.ToolCallID != "" {
				answered[e.ToolCallID] = true
			}
			if e.Usage != nil {
				usage.InputTokens += e.Usage.InputTokens
				usage.OutputTokens += e.Usage.OutputTokens
				usage.Cost += e.Usage.Cost
				if e.Usage.CostAvailable {
					usage.CostAvailable = true
				}
			}
			msgs = append(msgs, llm.Message{
				Role:       llm.Role(e.Role),
				Content:    e.Content,
				ToolCalls:  e.ToolCalls,
				ToolCallID: e.ToolCallID,
				Name:       e.Name,
			})
		case "compaction":
			var c Compaction
			if err := json.Unmarshal(raw, &c); err != nil {
				log.Warn("session compaction skipped", "path", path, "line", lineNo, "err", err.Error())
				continue
			}
			msgs = applyCompaction(msgs, c)
		case "system_prompt":
			var p SystemPrompt
			if err := json.Unmarshal(raw, &p); err != nil {
				log.Warn("session system prompt skipped", "path", path, "line", lineNo, "err", err.Error())
				continue
			}
			msgs, _ = WithSystemPrompt(msgs, p.Content)
			loadedSkills = p.Skills
		case "footer":
			// Diagnostic only; ignored on read.
		default:
			log.Warn("session line skipped (unknown type)", "path", path, "line", lineNo, "type", probe.Type)
		}
	}

	return Transcript{
		Header:   hdr,
		Messages: elideDangling(msgs, answered),
		Usage:    usage,
		Skills:   loadedSkills,
	}, nil
}

// applyCompaction replaces the span a compaction covers with its summary,
// reproducing the conversation the agent continued with after the operator
// compacted it.
//
// A range that does not fit what has been rebuilt means the file and the reader
// disagree about the conversation — a message line that failed to parse on the
// way in, most likely. Loading the transcript uncompacted is a worse answer than
// loading it at all, so the range is clamped and the discrepancy logged.
func applyCompaction(msgs []llm.Message, c Compaction) []llm.Message {
	from := c.From
	through := c.Through
	if from < 0 {
		from = 0
	}
	if through > len(msgs) {
		through = len(msgs)
	}
	if from >= through {
		log.Warn("session compaction covers nothing", "from", c.From, "through", c.Through, "messages", len(msgs))
		return msgs
	}
	if from != c.From || through != c.Through {
		log.Warn("session compaction range clamped",
			"from", c.From, "through", c.Through, "messages", len(msgs))
	}

	summary := llm.Message{Role: llm.Role(c.Role), Content: c.Content}
	out := make([]llm.Message, 0, len(msgs)-(through-from)+1)
	out = append(out, msgs[:from]...)
	out = append(out, summary)
	return append(out, msgs[through:]...)
}

// WithSystemPrompt returns msgs opening with prompt as its system message, and
// whether that differs from what msgs opened with. The leading system message is
// replaced when there is one and prompt is put in front when there is not. The
// agent resumes through this and Load replays SystemPrompt lines through it, so
// the two cannot disagree about the conversation a later compaction's positions
// index into. The result never shares msgs' backing array.
func WithSystemPrompt(msgs []llm.Message, prompt string) ([]llm.Message, bool) {
	system := llm.Message{Role: llm.RoleSystem, Content: prompt}
	if len(msgs) == 0 || msgs[0].Role != llm.RoleSystem {
		return append([]llm.Message{system}, msgs...), true
	}
	out := append([]llm.Message(nil), msgs...)
	if out[0].Content == prompt {
		return out, false
	}
	out[0] = system
	return out, true
}

// elideDangling drops trailing assistant messages whose tool_calls were never
// answered. Walking from the end handles the crash-mid-tool-call case without
// disturbing valid earlier turns.
func elideDangling(msgs []llm.Message, answered map[string]bool) []llm.Message {
	for len(msgs) > 0 {
		last := msgs[len(msgs)-1]
		if last.Role != llm.RoleAssistant || len(last.ToolCalls) == 0 {
			break
		}
		dangling := false
		for _, tc := range last.ToolCalls {
			if !answered[tc.ID] {
				dangling = true
				break
			}
		}
		if !dangling {
			break
		}
		log.Info("eliding dangling assistant tool_calls from resumed session", "tool_calls", len(last.ToolCalls))
		msgs = msgs[:len(msgs)-1]
	}
	return msgs
}

// List returns the session ids found in the sessions directory dir in
// reverse-chronological order (newest first). Because ids are timestamp-
// prefixed, a descending lexical sort is chronological. A missing directory
// returns (nil, nil).
func List(dir string) ([]ID, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("session: read dir %s: %w", dir, err)
	}
	var ids []ID
	for _, e := range entries {
		if e.IsDir() || !strings.EqualFold(filepath.Ext(e.Name()), ".jsonl") {
			continue
		}
		ids = append(ids, ID(strings.TrimSuffix(e.Name(), filepath.Ext(e.Name()))))
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] > ids[j] })
	return ids, nil
}
