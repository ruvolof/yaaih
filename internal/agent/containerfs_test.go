package agent

import (
	"context"
	"fmt"
	"os"
	osexec "os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/ruvolof/yaaih/internal/config"
	"github.com/ruvolof/yaaih/internal/executor"
)

func TestResolveContainerPath(t *testing.T) {
	const root = "/workspace"
	for _, tc := range []struct {
		rel  string
		want string // "" means an error is expected
	}{
		{".", "/workspace"},
		{"", "/workspace"},
		{"a/b.txt", "/workspace/a/b.txt"},
		{"a/../b", "/workspace/b"},
		{"./deep/./dir/", "/workspace/deep/dir"},
		{"..", ""},
		{"../x", ""},
		{"a/../../x", ""},
		{"/etc/passwd", ""},
		{"/workspace/a", ""},
		{"esc/../../..", ""},
		{"name\nwith-newline", ""},
		{"name\x00with-nul", ""},
	} {
		got, err := resolveContainerPath(root, tc.rel)
		if tc.want == "" {
			if err == nil {
				t.Errorf("resolveContainerPath(%q) = %q, want an error", tc.rel, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("resolveContainerPath(%q): %v", tc.rel, err)
		} else if got != tc.want {
			t.Errorf("resolveContainerPath(%q) = %q, want %q", tc.rel, got, tc.want)
		}
	}
}

func TestSplitContainerFSResult(t *testing.T) {
	marker := containerFSMarker
	for _, tc := range []struct {
		name    string
		raw     string
		payload string
		status  string
		detail  string
		ok      bool
	}{
		{"payload with trailing newline", "line1\nline2\n\n" + marker + " ok 2\n", "line1\nline2\n", "ok", "2", true},
		{"payload without trailing newline", "alpha\nbeta\n" + marker + " ok 2\n", "alpha\nbeta", "ok", "2", true},
		{"empty payload", "\n" + marker + " ok 0\n", "", "ok", "0", true},
		{"error status", "\n" + marker + " err no such file\n", "", "err", "no such file", true},
		{"marker text inside the payload", marker + " ok 99\nreal\n\n" + marker + " ok 2\n", marker + " ok 99\nreal\n", "ok", "2", true},
		{"executor note after the status line", "x\n\n" + marker + " ok 1\n[8.0MB of output was dropped]\n", "x\n", "ok", "1", true},
		{"marker never arrived", "partial output only\n", "", "", "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			payload, status, detail, ok := splitContainerFSResult(tc.raw)
			if ok != tc.ok {
				t.Fatalf("ok = %v, want %v", ok, tc.ok)
			}
			if payload != tc.payload || status != tc.status || detail != tc.detail {
				t.Errorf("got (%q, %q, %q), want (%q, %q, %q)", payload, status, detail, tc.payload, tc.status, tc.detail)
			}
		})
	}
}

func TestDescribeWindowTellsTheModelHowToContinue(t *testing.T) {
	for _, tc := range []struct {
		name                 string
		offset, limit, total int
		want                 string
	}{
		{"more to come", 1, 500, 42318, "[app.log: lines 1-500 of 42318 — call again with offset 501 for the next page]\n"},
		{"final page", 42001, 500, 42318, "[app.log: lines 42001-42318 of 42318]\n"},
		{"whole file", 1, 500, 12, "[app.log: lines 1-12 of 12]\n"},
		{"past the end", 900, 500, 12, "[app.log: offset 900 is past the end; the file has 12 lines]\n"},
		{"empty", 1, 500, 0, "[app.log: empty file]\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := describeWindow("app.log", tc.offset, tc.limit, tc.total); got != tc.want {
				t.Errorf("describeWindow = %q, want %q", got, tc.want)
			}
		})
	}
}

// containerlessBash runs each command with a host bash in place of the sandbox
// session. The generated commands only use POSIX userland (sed, grep, ls,
// base64), so a host shell is a faithful stand-in for everything except the
// mount boundary itself, which only a real container can provide.
type containerlessBash struct{}

func (containerlessBash) Execute(ctx context.Context, cmd string) (string, error) {
	out, _ := osexec.CommandContext(ctx, "bash", "-c", cmd).CombinedOutput()
	return string(out), nil
}

func (containerlessBash) CWD() string     { return "/" }
func (containerlessBash) IsRunning() bool { return true }
func (containerlessBash) Close() error    { return nil }

// bashWorkspace returns a runner backed by the host's bash and a temp dir
// standing in for the container workspace. The commands target the GNU
// userland of the runtime container, so only Linux hosts run these tests.
func bashWorkspace(t *testing.T) (executor.Executor, string) {
	t.Helper()
	if runtime.GOOS != "linux" {
		t.Skip("the generated commands target the runtime container's GNU userland")
	}
	if _, err := osexec.LookPath("bash"); err != nil {
		t.Skip("bash not on PATH")
	}
	return containerlessBash{}, t.TempDir()
}

func writeLines(t *testing.T, dir, name string, count int) {
	t.Helper()
	var content strings.Builder
	for i := 1; i <= count; i++ {
		fmt.Fprintf(&content, "line %d\n", i)
	}
	if err := os.WriteFile(filepath.Join(dir, name), []byte(content.String()), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestContainerFileWriteThenReadRoundTrip(t *testing.T) {
	runner, root := bashWorkspace(t)

	result, err := containerFileWrite(t.Context(), runner, root, "notes/deep/file.txt", "alpha\nbeta")
	if err != nil {
		t.Fatal(err)
	}
	if result != "Wrote 10 bytes to notes/deep/file.txt" {
		t.Errorf("write result = %q", result)
	}

	got, err := containerFileRead(t.Context(), runner, root, "notes/deep/file.txt", 1, 50)
	if err != nil {
		t.Fatal(err)
	}
	want := "[notes/deep/file.txt: lines 1-2 of 2]\nalpha\nbeta"
	if got != want {
		t.Errorf("read back %q, want %q — a final line without a newline must survive the round trip", got, want)
	}
}

// Content is carried base64-encoded, so bytes that are meaningful to the shell
// or to the result parser must come back untouched.
func TestContainerFileWriteRoundTripsHostileContent(t *testing.T) {
	runner, root := bashWorkspace(t)
	content := "single ' and double \" quotes\n$(reboot); `id` \\ %s\n" +
		containerFSMarker + " ok 999\nünïcödé ✓\n"

	if _, err := containerFileWrite(t.Context(), runner, root, "tricky.txt", content); err != nil {
		t.Fatal(err)
	}

	onDisk, err := os.ReadFile(filepath.Join(root, "tricky.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if string(onDisk) != content {
		t.Errorf("on disk %q, want %q", onDisk, content)
	}

	got, err := containerFileRead(t.Context(), runner, root, "tricky.txt", 1, 50)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(got, content) {
		t.Errorf("read back %q, want it to end with the exact content", got)
	}
	if !strings.HasPrefix(got, "[tricky.txt: lines 1-4 of 4]\n") {
		t.Errorf("header = %q, want 4 lines counted", firstLine(got))
	}
}

func TestContainerFileReadPaginates(t *testing.T) {
	runner, root := bashWorkspace(t)
	writeLines(t, root, "numbers.txt", 1000)

	got, err := containerFileRead(t.Context(), runner, root, "numbers.txt", 101, 5)
	if err != nil {
		t.Fatal(err)
	}
	want := "[numbers.txt: lines 101-105 of 1000 — call again with offset 106 for the next page]\n" +
		"line 101\nline 102\nline 103\nline 104\nline 105\n"
	if got != want {
		t.Errorf("page = %q, want %q", got, want)
	}

	clamped, err := containerFileRead(t.Context(), runner, root, "numbers.txt", 999, 500)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(clamped, "[numbers.txt: lines 999-1000 of 1000]\n") {
		t.Errorf("clamped header = %q", firstLine(clamped))
	}

	past, err := containerFileRead(t.Context(), runner, root, "numbers.txt", 5000, 10)
	if err != nil {
		t.Fatalf("reading past the end should describe the file, not fail: %v", err)
	}
	if past != "[numbers.txt: offset 5000 is past the end; the file has 1000 lines]\n" {
		t.Errorf("past-the-end result = %q", past)
	}
}

func TestContainerFileReadEmptyFile(t *testing.T) {
	runner, root := bashWorkspace(t)
	if err := os.WriteFile(filepath.Join(root, "empty.txt"), nil, 0o644); err != nil {
		t.Fatal(err)
	}

	got, err := containerFileRead(t.Context(), runner, root, "empty.txt", 1, 50)
	if err != nil {
		t.Fatal(err)
	}
	if got != "[empty.txt: empty file]\n" {
		t.Errorf("result = %q", got)
	}
}

// A file the harness can write is a file it must be able to read back, even
// when it is one enormous line.
func TestContainerFileReadHandlesVeryLongLines(t *testing.T) {
	runner, root := bashWorkspace(t)
	giant := strings.Repeat("x", 20<<20)
	if err := os.WriteFile(filepath.Join(root, "oneline.json"), []byte(giant+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	got, err := containerFileRead(t.Context(), runner, root, "oneline.json", 1, 10)
	if err != nil {
		t.Fatal(err)
	}
	want := "[oneline.json: lines 1-1 of 1]\n" + giant + "\n"
	if got != want {
		t.Errorf("len(got) = %d, want %d with header", len(got), len(want))
	}
}

func TestContainerFileReadErrors(t *testing.T) {
	runner, root := bashWorkspace(t)
	if err := os.Mkdir(filepath.Join(root, "adir"), 0o755); err != nil {
		t.Fatal(err)
	}

	if _, err := containerFileRead(t.Context(), runner, root, "missing.txt", 1, 50); err == nil || !strings.Contains(err.Error(), "no such file") {
		t.Errorf("missing file error = %v", err)
	}
	if _, err := containerFileRead(t.Context(), runner, root, "adir", 1, 50); err == nil || !strings.Contains(err.Error(), "is a directory") {
		t.Errorf("directory error = %v", err)
	}
}

func TestContainerFileWriteErrors(t *testing.T) {
	runner, root := bashWorkspace(t)
	if err := os.Mkdir(filepath.Join(root, "adir"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "plain.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	if _, err := containerFileWrite(t.Context(), runner, root, "adir", "content"); err == nil || !strings.Contains(err.Error(), "is a directory") {
		t.Errorf("directory target error = %v", err)
	}
	// mkdir -p cannot make a directory out of an existing file; its stderr is
	// what the model gets to see.
	if _, err := containerFileWrite(t.Context(), runner, root, "plain.txt/under", "content"); err == nil {
		t.Error("writing under a path that is a file should fail")
	}
}

func TestContainerFileList(t *testing.T) {
	runner, root := bashWorkspace(t)
	for _, name := range []string{"a.txt", ".hidden"} {
		if err := os.WriteFile(filepath.Join(root, name), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Mkdir(filepath.Join(root, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}

	got, err := containerFileList(t.Context(), runner, root, ".")
	if err != nil {
		t.Fatal(err)
	}
	if got != "f  .hidden\nf  a.txt\nd  sub\n" {
		t.Errorf("listing = %q", got)
	}

	empty, err := containerFileList(t.Context(), runner, root, "sub")
	if err != nil {
		t.Fatal(err)
	}
	if empty != "" {
		t.Errorf("empty dir listing = %q", empty)
	}

	if _, err := containerFileList(t.Context(), runner, root, "missing"); err == nil || !strings.Contains(err.Error(), "no such directory") {
		t.Errorf("missing dir error = %v", err)
	}
	if _, err := containerFileList(t.Context(), runner, root, "a.txt"); err == nil || !strings.Contains(err.Error(), "not a directory") {
		t.Errorf("file target error = %v", err)
	}
}

// The mount is the boundary a real container enforces; what the harness itself
// must guarantee is that a lexical escape never even reaches the sandbox.
func TestFileToolsRejectEscapesBeforeTheSandbox(t *testing.T) {
	runner := &stubRunner{}
	registry := NewRegistry(&config.Config{Options: config.Options{MaxFileReadLines: 50}}, runner, nil)

	for _, call := range []struct{ tool, args string }{
		{"file_read", `{"path":"../secrets"}`},
		{"file_read", `{"path":"/etc/passwd"}`},
		{"file_write", `{"path":"esc/../../.bashrc","content":"x"}`},
		{"file_list", `{"path":".."}`},
	} {
		tool, ok := registry.Enabled(call.tool, []string{"filesystem"})
		if !ok {
			t.Fatalf("%s not registered", call.tool)
		}
		if _, err := tool.Fn(t.Context(), call.args); err == nil {
			t.Errorf("%s %s: no error for a path outside the workspace", call.tool, call.args)
		}
	}
	if executed := runner.executed(); len(executed) != 0 {
		t.Errorf("escaping paths reached the sandbox: %q", executed)
	}
}

// End to end through the registry: the tool applies the default page size,
// addresses the file under the container workspace, and prefixes the window
// with its header.
func TestFileReadToolCommandsAndComposesResult(t *testing.T) {
	runner := &stubRunner{output: "hello\n\n" + containerFSMarker + " ok 1\n"}
	registry := NewRegistry(&config.Config{Options: config.Options{MaxFileReadLines: 50}}, runner, nil)

	tool, ok := registry.Enabled("file_read", []string{"filesystem"})
	if !ok {
		t.Fatal("file_read not registered")
	}
	got, err := tool.Fn(t.Context(), `{"path":"hello.txt"}`)
	if err != nil {
		t.Fatal(err)
	}
	if got != "[hello.txt: lines 1-1 of 1]\nhello\n" {
		t.Errorf("result = %q", got)
	}

	commands := runner.executed()
	if len(commands) != 1 {
		t.Fatalf("executed %d commands, want 1", len(commands))
	}
	if !strings.Contains(commands[0], "sed -n '1,50p'") {
		t.Errorf("command does not window with the default page size: %q", commands[0])
	}
	if !strings.Contains(commands[0], b64Arg("/workspace/hello.txt")) {
		t.Errorf("command does not address the file under the container workspace: %q", commands[0])
	}
}

func firstLine(s string) string {
	line, _, _ := strings.Cut(s, "\n")
	return line
}
