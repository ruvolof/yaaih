// Package browser wraps playwright-go and connects to a Chromium instance
// running inside the harness runtime container via CDP.
//
// Lifecycle:
//
//  1. Browser is created with the CDP endpoint published by the docker
//     package (e.g. http://127.0.0.1:<random>).
//  2. First operation launches Chromium + a socat forwarder inside the
//     container (via one-shot Docker exec), installs the playwright-go
//     driver on the host if needed, and connects over CDP.
//  3. Subsequent operations reuse the connection.
//  4. Close() detaches playwright and tears down the in-container processes.
//
// Vision support is honoured at the tool layer: when the model lacks vision,
// screenshot calls return a Markdown text summary of the page in place of an
// image path.
package browser

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/client"
	"github.com/docker/docker/pkg/stdcopy"
	"github.com/mxschmitt/playwright-go"

	"github.com/ruvolof/aibitat-harness-go/internal/logging"
)

var log = logging.For("browser")

// Options configures the browser.
type Options struct {
	// CDPEndpoint is the URL playwright connects to (e.g. http://127.0.0.1:34567).
	CDPEndpoint string
	// DockerClient and ContainerID are used to launch Chromium inside the
	// runtime container when the browser is first needed.
	DockerClient *client.Client
	ContainerID  string
	// ScreenshotsDir is the host directory where screenshots are saved. Must
	// be writable and ideally on the workspace bind-mount so paths are
	// addressable from inside the container too.
	ScreenshotsDir string
	// SupportsVision flips Screenshot from "return image path" to "also return
	// a text fallback the model can consume".
	SupportsVision bool
}

// Browser is a lazily-initialised handle.
type Browser struct {
	opts Options

	mu      sync.Mutex
	pw      *playwright.Playwright
	browser playwright.Browser
	page    playwright.Page

	closed bool
}

// New returns an unstarted Browser. The first operation triggers initialisation.
func New(opts Options) *Browser { return &Browser{opts: opts} }

// init lazily launches Chromium inside the container and connects via CDP.
// Safe to call multiple times — only the first call does work.
func (b *Browser) init(ctx context.Context) error {
	if b.browser != nil {
		return nil
	}
	if b.opts.CDPEndpoint == "" {
		return errors.New("browser: CDPEndpoint is empty (runtime container not started?)")
	}
	if b.opts.DockerClient == nil || b.opts.ContainerID == "" {
		return errors.New("browser: missing docker client or container id")
	}

	// 1. Locate the Chromium binary inside the image. The playwright base ships
	//    it under chromium-XXX/, in a directory whose name tracks the upstream
	//    build layout: chrome-linux/ on older releases, chrome-linux64/ since
	//    Chrome-for-Testing. The headless-shell build sits alongside it but
	//    names its binary headless_shell/chrome-headless-shell, so it never
	//    matches here.
	//    Failing fast here beats a 10 s CDP timeout when the image changes.
	chromePath := strings.TrimSpace(b.execCapture(ctx,
		"ls /ms-playwright/*/chrome-linux*/chrome 2>/dev/null | head -n1"))
	if chromePath == "" {
		return errors.New("browser: chromium binary not found under /ms-playwright/*/chrome-linux*/chrome — did the runtime image change?")
	}
	log.Info("found chromium", "path", chromePath)

	// 2. Start headless Chromium on 127.0.0.1:9223 inside the container and
	//    expose it via socat on 0.0.0.0:9222 so Docker's port mapping can
	//    reach it.
	//
	//    Chrome 132 silently ignores --remote-debugging-address=0.0.0.0
	//    for security and binds only to loopback; Docker's port-mapper
	//    forwards from the host into the container's *bridge* interface,
	//    not the container's loopback, so direct port mapping can never
	//    reach Chromium. socat bridges the two and as a side-benefit makes
	//    every incoming connection look like 127.0.0.1 to Chromium.
	//
	//    Notes on the script:
	//      - '*' is single-quoted: the exec's WorkingDir is /workspace and
	//        an unquoted * would be shell-globbed against its contents.
	//      - statements are newline-separated; "; " after `&` is a shell
	//        syntax error.
	startCmd := strings.Join([]string{
		"set -e",
		fmt.Sprintf(`nohup %q --headless=new --no-sandbox --disable-dev-shm-usage`+
			` --remote-debugging-address=127.0.0.1 --remote-debugging-port=9223`+
			` '--remote-allow-origins=*'`+
			` about:blank >/tmp/chromium.log 2>&1 &`, chromePath),
		`nohup socat TCP-LISTEN:9222,bind=0.0.0.0,fork,reuseaddr TCP:127.0.0.1:9223 >/tmp/socat.log 2>&1 &`,
		"sleep 0.5",
	}, "\n")

	execResp, err := b.opts.DockerClient.ContainerExecCreate(ctx, b.opts.ContainerID, container.ExecOptions{
		Cmd:    []string{"sh", "-c", startCmd},
		Detach: true,
	})
	if err != nil {
		return fmt.Errorf("browser: exec create: %w", err)
	}
	if err := b.opts.DockerClient.ContainerExecStart(ctx, execResp.ID, container.ExecStartOptions{Detach: true}); err != nil {
		return fmt.Errorf("browser: exec start: %w", err)
	}

	// 3. Install the playwright-go driver on the host if needed, then run it.
	//    Install is idempotent — first call downloads ~50 MB into the user's
	//    cache; subsequent calls short-circuit on a version check. We pass
	//    SkipInstallBrowsers because Chromium lives in the container.
	if err := playwright.Install(&playwright.RunOptions{SkipInstallBrowsers: true}); err != nil {
		return fmt.Errorf("browser: install playwright driver: %w", err)
	}
	pw, err := playwright.Run()
	if err != nil {
		return fmt.Errorf("browser: playwright run: %w", err)
	}

	// CDP server takes a moment to come up after the chrome process spawns.
	var br playwright.Browser
	deadline := time.Now().Add(10 * time.Second)
	for {
		br, err = pw.Chromium.ConnectOverCDP(b.opts.CDPEndpoint)
		if err == nil {
			break
		}
		if time.Now().After(deadline) {
			// Pull diagnostic state out of the container so failures are
			// debuggable from the harness log alone — no separate
			// `docker exec` chase needed. Best-effort: any failure to
			// read becomes an empty string.
			log.Error("CDP connect failed; dumping container diagnostics",
				"endpoint", b.opts.CDPEndpoint,
				"err", err,
				"chromium_log", b.readChromiumLog(ctx),
				"socat_log", b.execCapture(ctx, "tail -c 2048 /tmp/socat.log 2>/dev/null || true"),
				"listening_ports", b.execCapture(ctx, "ss -tln 2>/dev/null || netstat -tln 2>/dev/null || true"),
			)
			_ = pw.Stop()
			return fmt.Errorf("browser: connect over cdp: %w", err)
		}
		select {
		case <-ctx.Done():
			_ = pw.Stop()
			return ctx.Err()
		case <-time.After(200 * time.Millisecond):
		}
	}

	// Reuse the default context/page that ConnectOverCDP exposes.
	ctxs := br.Contexts()
	var bctx playwright.BrowserContext
	if len(ctxs) > 0 {
		bctx = ctxs[0]
	} else {
		bctx, err = br.NewContext()
		if err != nil {
			_ = br.Close()
			_ = pw.Stop()
			return fmt.Errorf("browser: new context: %w", err)
		}
	}
	pages := bctx.Pages()
	var page playwright.Page
	if len(pages) > 0 {
		page = pages[0]
	} else {
		page, err = bctx.NewPage()
		if err != nil {
			_ = br.Close()
			_ = pw.Stop()
			return fmt.Errorf("browser: new page: %w", err)
		}
	}

	b.pw = pw
	b.browser = br
	b.page = page

	if err := os.MkdirAll(b.opts.ScreenshotsDir, 0o755); err != nil {
		return fmt.Errorf("browser: mkdir screenshots: %w", err)
	}
	log.Info("connected via CDP",
		"endpoint", b.opts.CDPEndpoint,
		"vision", b.opts.SupportsVision,
		"screenshots_dir", b.opts.ScreenshotsDir,
	)
	return nil
}

// Navigate loads url and waits for network idle.
func (b *Browser) Navigate(ctx context.Context, url string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if err := b.init(ctx); err != nil {
		return err
	}
	start := time.Now()
	log.Info("navigate", "url", url)
	_, err := b.page.Goto(url, playwright.PageGotoOptions{
		WaitUntil: playwright.WaitUntilStateNetworkidle,
	})
	if err != nil {
		log.Error("navigate failed", "url", url, "err", err, "elapsed_ms", time.Since(start).Milliseconds())
	} else {
		log.Info("navigate done", "url", url, "elapsed_ms", time.Since(start).Milliseconds())
	}
	return err
}

// Content returns the current page rendered as Markdown — stripped of script
// and style tags, with headings, paragraphs, and links preserved.
func (b *Browser) Content(ctx context.Context) (string, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if err := b.init(ctx); err != nil {
		return "", err
	}
	// Use a small in-page script that walks the DOM and produces a Markdown summary.
	// This is intentionally simple: heuristics, not perfect reproduction.
	js := `() => {
        const drop = new Set(['SCRIPT','STYLE','NOSCRIPT','SVG','PATH','META','LINK']);
        const out = [];
        function walk(node) {
            if (!node) return;
            if (node.nodeType === Node.TEXT_NODE) {
                const t = node.textContent.trim();
                if (t) out.push(t);
                return;
            }
            if (node.nodeType !== Node.ELEMENT_NODE) return;
            if (drop.has(node.tagName)) return;
            const tag = node.tagName;
            if (/^H[1-6]$/.test(tag)) {
                const n = parseInt(tag[1]);
                out.push('\n' + '#'.repeat(n) + ' ' + (node.innerText || '').trim() + '\n');
                return;
            }
            if (tag === 'A') {
                const href = node.getAttribute('href') || '';
                const text = (node.innerText || '').trim();
                if (text) out.push('[' + text + '](' + href + ')');
                return;
            }
            if (tag === 'LI') { out.push('\n- '); }
            if (tag === 'P' || tag === 'DIV' || tag === 'BR') { out.push('\n'); }
            for (const c of node.childNodes) walk(c);
        }
        walk(document.body);
        return out.join(' ').replace(/[ \t]+/g, ' ').replace(/\n[ \t]+/g, '\n').replace(/\n{3,}/g, '\n\n');
    }`
	res, err := b.page.Evaluate(js)
	if err != nil {
		return "", err
	}
	s, _ := res.(string)
	return s, nil
}

// Screenshot saves a PNG of the current page to ScreenshotsDir and returns the
// absolute host path. When SupportsVision is false, the second return value is
// a text fallback the agent loop should pass to the model instead.
func (b *Browser) Screenshot(ctx context.Context) (path string, textFallback string, err error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if err := b.init(ctx); err != nil {
		return "", "", err
	}
	name := fmt.Sprintf("screenshot-%d.png", time.Now().UnixNano())
	full := filepath.Join(b.opts.ScreenshotsDir, name)
	if _, err := b.page.Screenshot(playwright.PageScreenshotOptions{
		Path:     playwright.String(full),
		FullPage: playwright.Bool(true),
	}); err != nil {
		log.Error("screenshot failed", "err", err)
		return "", "", err
	}
	log.Info("screenshot saved", "path", full, "vision", b.opts.SupportsVision)
	if b.opts.SupportsVision {
		return full, "", nil
	}
	// Text fallback when the model can't process images.
	// Reuse Content() but be careful: we already hold the lock; call the
	// unlocked inner version.
	js := `() => document.title + '\n\n' + document.body.innerText`
	res, _ := b.page.Evaluate(js)
	s, _ := res.(string)
	return full, s, nil
}

// Click clicks the element matching the given selector.
func (b *Browser) Click(ctx context.Context, selector string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if err := b.init(ctx); err != nil {
		return err
	}
	return b.page.Click(selector)
}

// Fill sets the value of the element matching selector. The name mirrors
// Playwright's underlying API: this assigns the field's value directly rather
// than simulating individual keystrokes.
func (b *Browser) Fill(ctx context.Context, selector, text string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if err := b.init(ctx); err != nil {
		return err
	}
	return b.page.Fill(selector, text)
}

// Eval executes JS in the page context and returns the string-coerced result.
func (b *Browser) Eval(ctx context.Context, script string) (string, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if err := b.init(ctx); err != nil {
		return "", err
	}
	res, err := b.page.Evaluate(script)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("%v", res), nil
}

// ListTabs returns one-line descriptors of every open page in the connected
// browser context.
func (b *Browser) ListTabs(ctx context.Context) ([]string, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if err := b.init(ctx); err != nil {
		return nil, err
	}
	out := []string{}
	for i, p := range b.page.Context().Pages() {
		title, _ := p.Title()
		out = append(out, fmt.Sprintf("[%d] %s — %s", i, title, p.URL()))
	}
	return out, nil
}

// CloseTab closes the tab at the given index in the current context.
func (b *Browser) CloseTab(ctx context.Context, index int) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if err := b.init(ctx); err != nil {
		return err
	}
	pages := b.page.Context().Pages()
	if index < 0 || index >= len(pages) {
		return fmt.Errorf("browser: tab index %d out of range", index)
	}
	return pages[index].Close()
}

// execCapture runs a one-shot command inside the runtime container and
// returns its merged stdout/stderr, capped at 4 KiB to keep log lines
// bounded. Best-effort: any failure returns "".
//
// Used for diagnostics (locating the chromium binary, dumping chromium.log
// when CDP fails) — not for anything on a hot path.
func (b *Browser) execCapture(ctx context.Context, command string) string {
	if b.opts.DockerClient == nil || b.opts.ContainerID == "" {
		return ""
	}
	cctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()

	idResp, err := b.opts.DockerClient.ContainerExecCreate(cctx, b.opts.ContainerID, container.ExecOptions{
		Cmd:          []string{"sh", "-c", command},
		AttachStdout: true,
		AttachStderr: true,
	})
	if err != nil {
		return ""
	}
	hijack, err := b.opts.DockerClient.ContainerExecAttach(cctx, idResp.ID, container.ExecStartOptions{})
	if err != nil {
		return ""
	}
	defer hijack.Close()

	var buf bytes.Buffer
	// stdcopy demuxes the docker exec multiplexed stream into stdout/stderr;
	// we merge both into the same buffer because callers want the full
	// output and don't care which fd it came from.
	_, _ = stdcopy.StdCopy(&buf, &buf, hijack.Reader)
	if buf.Len() > 4096 {
		buf.Truncate(4096)
	}
	return buf.String()
}

// readChromiumLog returns the last 4 KiB of /tmp/chromium.log from the
// runtime container. Empty on any failure.
func (b *Browser) readChromiumLog(ctx context.Context) string {
	return strings.TrimSpace(b.execCapture(ctx,
		"tail -c 4096 /tmp/chromium.log 2>/dev/null || true"))
}

// Close detaches playwright and signals the in-container Chromium to exit.
func (b *Browser) Close() error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed {
		return nil
	}
	b.closed = true
	var firstErr error
	if b.browser != nil {
		if err := b.browser.Close(); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	if b.pw != nil {
		if err := b.pw.Stop(); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	// Best-effort kill of the in-container Chromium and the socat forwarder
	// we launched alongside it, so neither lingers if the session is
	// reused. The container itself is torn down by the docker package on
	// shutdown, so this is belt-and-braces.
	if b.opts.DockerClient != nil && b.opts.ContainerID != "" {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		idResp, err := b.opts.DockerClient.ContainerExecCreate(ctx, b.opts.ContainerID, container.ExecOptions{
			Cmd:    []string{"sh", "-c", "pkill -f chrome 2>/dev/null; pkill -f socat 2>/dev/null; true"},
			Detach: true,
		})
		if err == nil {
			_ = b.opts.DockerClient.ContainerExecStart(ctx, idResp.ID, container.ExecStartOptions{Detach: true})
		}
	}
	return firstErr
}
