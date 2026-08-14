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
	"strconv"
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
	// UserAgent overrides the User-Agent Chromium advertises. Empty derives one
	// from the version of the Chromium in the container.
	UserAgent string
	// Timeout bounds navigation and element operations. Zero leaves
	// playwright's own 30 s default in place.
	Timeout time.Duration
}

// Browser is a lazily-initialised handle.
type Browser struct {
	opts Options

	mu      sync.Mutex
	pw      *playwright.Playwright
	browser playwright.Browser
	// bctx owns the tabs. Tab operations go through it rather than through
	// page.Context() so that closing the active tab does not leave them
	// reaching through a dead page.
	bctx playwright.BrowserContext
	page playwright.Page

	closed bool
}

// New returns an unstarted Browser. The first operation triggers initialisation.
func New(opts Options) *Browser { return &Browser{opts: opts} }

// automationHidingScript runs before any page script and clears the handful of
// globals that mark a CDP-driven browser as automated. It is deliberately
// short: the goal is to stop a routine fingerprinting check from serving a
// challenge page to an agent browsing on its operator's behalf, not to defeat
// dedicated anti-bot systems. A sprawling stealth bundle is itself detectable
// and is not worth maintaining here.
const automationHidingScript = `
delete Object.getPrototypeOf(navigator).webdriver;
Object.defineProperty(navigator, 'languages', { get: () => ['en-US', 'en'] });
window.chrome = window.chrome || { runtime: {} };
`

// fallbackChromiumMajor stands in when the version probe fails. A UA claiming a
// version the engine does not have is itself a fingerprint, so this is a last
// resort rather than the normal path.
const fallbackChromiumMajor = "140"

// desktopUserAgent builds the UA string for a Chromium of the given major
// version on desktop Linux.
func desktopUserAgent(major string) string {
	if major == "" {
		major = fallbackChromiumMajor
	}
	return fmt.Sprintf("Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 (KHTML, like Gecko) "+
		"Chrome/%s.0.0.0 Safari/537.36", major)
}

// chromiumMajorVersion asks the container's Chromium what it is, so the UA the
// harness advertises tracks the engine actually running rather than drifting
// away from it on every image rebuild. Returns "" when the probe fails.
func (b *Browser) chromiumMajorVersion(ctx context.Context, chromePath string) string {
	banner := b.execCapture(ctx, fmt.Sprintf("%q --version 2>/dev/null", chromePath))
	major := parseChromiumMajor(banner)
	if major == "" {
		log.Warn("chromium version probe failed; falling back to a fixed user agent", "output", banner)
	}
	return major
}

// parseChromiumMajor pulls the major version out of a --version banner
// ("Chromium 141.0.7390.54"). Returns "" for anything it does not recognise.
func parseChromiumMajor(banner string) string {
	fields := strings.Fields(banner)
	if len(fields) == 0 {
		return ""
	}
	major, _, _ := strings.Cut(fields[len(fields)-1], ".")
	if major == "" || strings.TrimLeft(major, "0123456789") != "" {
		return ""
	}
	return major
}

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

	userAgent := b.opts.UserAgent
	if userAgent == "" {
		userAgent = desktopUserAgent(b.chromiumMajorVersion(ctx, chromePath))
	}
	log.Info("user agent", "value", userAgent, "overridden", b.opts.UserAgent != "")

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
	//      - the ';' in --accept-lang is single-quoted for the same reason it
	//        would need to be at a prompt: bare, it ends the statement.
	//      - statements are newline-separated; "; " after `&` is a shell
	//        syntax error.
	//
	//    The presentation flags are not cosmetic. Without --user-agent the
	//    UA still reads HeadlessChrome, which alone is enough for a commercial
	//    bot filter to serve a challenge page instead of content; without a
	//    window size the viewport is 800x600, which serves mobile layouts the
	//    model then has to reason about.
	startCmd := strings.Join([]string{
		"set -e",
		fmt.Sprintf(`nohup %q --headless=new --no-sandbox --disable-dev-shm-usage`+
			` --remote-debugging-address=127.0.0.1 --remote-debugging-port=9223`+
			` '--remote-allow-origins=*'`+
			` --disable-blink-features=AutomationControlled`+
			` --window-size=1920,1080 --lang=en-US '--accept-lang=en-US,en;q=0.9'`+
			` --user-agent=%q`+
			` about:blank >/tmp/chromium.log 2>&1 &`, chromePath, userAgent),
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
	if b.opts.Timeout > 0 {
		ms := float64(b.opts.Timeout.Milliseconds())
		bctx.SetDefaultTimeout(ms)
		bctx.SetDefaultNavigationTimeout(ms)
	}
	if err := bctx.AddInitScript(playwright.Script{Content: playwright.String(automationHidingScript)}); err != nil {
		log.Warn("add init script failed; automation markers remain visible to the page", "err", err)
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
	b.bctx = bctx
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

// Navigate loads url and returns once the DOM is parsed.
//
// It deliberately does not wait for the network to fall idle: analytics
// beacons, websockets, and long-polling keep a request in flight indefinitely
// on most modern pages, so waiting for idle turns an ordinary page load into a
// timeout that reads like a blocked request. Content that arrives after
// DOMContentLoaded is the caller's business — see WaitFor.
func (b *Browser) Navigate(ctx context.Context, url string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if err := b.init(ctx); err != nil {
		return err
	}
	start := time.Now()
	log.Info("navigate", "url", url)
	_, err := b.page.Goto(url, playwright.PageGotoOptions{
		WaitUntil: playwright.WaitUntilStateDomcontentloaded,
	})
	if err != nil {
		log.Error("navigate failed", "url", url, "err", err, "elapsed_ms", time.Since(start).Milliseconds())
	} else {
		log.Info("navigate done", "url", url, "elapsed_ms", time.Since(start).Milliseconds())
	}
	return err
}

// WaitFor blocks until the element matching selector reaches state, or the
// browser timeout expires. An empty state waits for the element to become
// visible.
//
// This is the deliberate half of navigation: Navigate returns as soon as the
// DOM is parsed, so anything a script renders afterwards is waited for here, by
// name, instead of being guessed at with a blanket network-idle wait.
func (b *Browser) WaitFor(ctx context.Context, selector, state string) error {
	waitState, err := waitForSelectorState(state)
	if err != nil {
		return err
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if err := b.init(ctx); err != nil {
		return err
	}
	start := time.Now()
	_, err = b.page.WaitForSelector(selector, playwright.PageWaitForSelectorOptions{State: waitState})
	log.Info("wait for selector",
		"selector", selector,
		"state", *waitState,
		"err", err,
		"elapsed_ms", time.Since(start).Milliseconds(),
	)
	return err
}

func waitForSelectorState(state string) (*playwright.WaitForSelectorState, error) {
	switch state {
	case "", "visible":
		return playwright.WaitForSelectorStateVisible, nil
	case "attached":
		return playwright.WaitForSelectorStateAttached, nil
	case "detached":
		return playwright.WaitForSelectorStateDetached, nil
	case "hidden":
		return playwright.WaitForSelectorStateHidden, nil
	default:
		return nil, fmt.Errorf("browser: unknown wait state %q (want visible, hidden, attached, or detached)", state)
	}
}

// Content returns the current page rendered as Markdown — boilerplate dropped,
// with headings, lists, tables, code blocks, and links preserved.
func (b *Browser) Content(ctx context.Context) (string, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if err := b.init(ctx); err != nil {
		return "", err
	}
	return b.content()
}

// content is Content without the lock, for callers that already hold it.
func (b *Browser) content() (string, error) {
	res, err := b.page.Evaluate(strings.ReplaceAll(contentScript, "__MAX_CHARS__", strconv.Itoa(maxContentChars)))
	if err != nil {
		return "", err
	}
	text, _ := res.(string)
	return text, nil
}

// maxContentChars bounds what one extraction collects before it gives up on the
// rest of the page.
//
// This is not the conversation budget — options.max_tool_result_bytes decides
// how much of the result the model actually sees, and spills the remainder to
// disk. This bound exists so that a pathological page cannot push megabytes of
// DOM text across CDP and through the host just to be cut down afterwards.
const maxContentChars = 200_000

// contentScript walks the DOM and renders Markdown. __MAX_CHARS__ is
// substituted before evaluation.
//
// The script is a Go raw string, which cannot contain a backtick, so the one
// Markdown needs for code spans and fences is spelled with the JavaScript
// escape \u0060 and assigned to TICK.
//
// Heuristics, not faithful reproduction. Two properties are worth preserving if
// this is ever edited:
//
//   - Fragments are concatenated, never joined with a separator. Inline flow
//     comes from the source's own whitespace, collapsed per text node, so
//     punctuation stays attached to the word before it.
//   - Structure is emitted as newlines through breakLine, which tops the buffer
//     up to the requested depth rather than appending blindly, so nested block
//     elements do not stack blank lines.
const contentScript = `() => {
    const MAX = __MAX_CHARS__;
    const TICK = '\u0060';
    const FENCE = TICK + TICK + TICK;

    const dropped = new Set(['SCRIPT','STYLE','NOSCRIPT','SVG','PATH','META','LINK',
        'TEMPLATE','IFRAME','CANVAS','AUDIO','VIDEO','NAV','FOOTER','ASIDE']);
    const paragraphs = new Set(['P','BLOCKQUOTE','FIGURE','TABLE','UL','OL','DL','FORM','FIELDSET']);
    const containers = new Set(['ADDRESS','ARTICLE','DD','DIV','DT','FIGCAPTION','HEADER','MAIN','SECTION']);

    let buffer = '';
    let truncated = false;

    function emit(text) {
        if (truncated || !text) return;
        if (buffer.length + text.length > MAX) {
            buffer += text.slice(0, MAX - buffer.length);
            truncated = true;
            return;
        }
        buffer += text;
    }

    function emitInline(raw) {
        let text = raw.replace(/\s+/g, ' ');
        if (!buffer || /\s$/.test(buffer)) text = text.replace(/^ /, '');
        emit(text);
    }

    function breakLine(depth) {
        if (!buffer) return;
        let have = 0;
        while (have < buffer.length && buffer[buffer.length - 1 - have] === '\n') have++;
        for (; have < depth; have++) emit('\n');
    }

    function walkChildren(node) {
        for (const child of node.childNodes) walk(child);
    }

    function linkTarget(node) {
        const href = node.href || node.getAttribute('href') || '';
        if (!href || href.length > 400 || href.startsWith('javascript:')) return '';
        return href;
    }

    function walk(node) {
        if (truncated) return;
        if (node.nodeType === Node.TEXT_NODE) { emitInline(node.textContent); return; }
        if (node.nodeType !== Node.ELEMENT_NODE) return;

        const tag = node.tagName;
        if (dropped.has(tag)) return;
        if (node.hasAttribute('hidden') || node.getAttribute('aria-hidden') === 'true') return;

        if (tag === 'BR') { breakLine(1); return; }
        if (tag === 'HR') { breakLine(2); emit('---'); breakLine(2); return; }

        if (tag === 'PRE') {
            breakLine(2);
            emit(FENCE + '\n' + node.textContent.replace(/\s+$/, '') + '\n' + FENCE);
            breakLine(2);
            return;
        }
        if (tag === 'CODE') {
            const text = node.textContent.replace(/\s+/g, ' ').trim();
            if (text) emit(TICK + text + TICK);
            return;
        }
        if (/^H[1-6]$/.test(tag)) {
            breakLine(2);
            emit('#'.repeat(Number(tag[1])) + ' ');
            walkChildren(node);
            breakLine(2);
            return;
        }
        if (tag === 'A') {
            const text = node.textContent.replace(/\s+/g, ' ').trim();
            if (!text) return;
            const href = linkTarget(node);
            emit(href ? '[' + text + '](' + href + ')' : text);
            return;
        }
        if (tag === 'LI') {
            breakLine(1);
            const parent = node.parentElement;
            const ordered = parent && parent.tagName === 'OL';
            emit(ordered ? (Array.prototype.indexOf.call(parent.children, node) + 1) + '. ' : '- ');
            walkChildren(node);
            breakLine(1);
            return;
        }
        if (tag === 'TR') { breakLine(1); walkChildren(node); breakLine(1); return; }
        if (tag === 'TD' || tag === 'TH') {
            if (buffer && !/\n$/.test(buffer)) emit(' | ');
            walkChildren(node);
            return;
        }
        if (paragraphs.has(tag)) { breakLine(2); walkChildren(node); breakLine(2); return; }
        if (containers.has(tag)) { breakLine(1); walkChildren(node); breakLine(1); return; }

        walkChildren(node);
    }

    function extract(root) {
        buffer = '';
        truncated = false;
        walk(root);
        return buffer.replace(/[ \t]+\n/g, '\n').replace(/\n{3,}/g, '\n\n').trim();
    }

    const body = document.body || document.documentElement;
    // A main/article wrapper is the page telling us where its content is. Some
    // are empty shells filled in later, so a suspiciously short result falls
    // back to the whole body rather than reporting the page as blank.
    const preferred = document.querySelector('main') || document.querySelector('article');
    let text = preferred ? extract(preferred) : '';
    if (text.length < 200) text = extract(body);
    if (truncated) text += '\n\n[content truncated at ' + MAX + ' characters]';

    const header = [];
    if (document.title) header.push('# ' + document.title);
    header.push('URL: ' + location.href);
    return header.join('\n') + '\n\n' + text;
}`

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
	text, err := b.content()
	if err != nil {
		log.Warn("screenshot text fallback failed", "err", err)
	}
	return full, text, nil
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

// Back returns the active tab to the previous entry in its history and reports
// the URL it landed on.
func (b *Browser) Back(ctx context.Context) (string, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if err := b.init(ctx); err != nil {
		return "", err
	}
	before := b.page.URL()
	// Waiting for a lifecycle event here would hang until the timeout: going
	// back usually restores the page from the back/forward cache, and a
	// restored document fires neither DOMContentLoaded nor load again.
	response, err := b.page.GoBack(playwright.PageGoBackOptions{
		WaitUntil: playwright.WaitUntilStateCommit,
	})
	if err != nil {
		return "", err
	}
	after := b.page.URL()
	// A restore from that cache reports no response either, as does stepping
	// back out of an anchor, so an unchanged URL — not a nil response — is what
	// actually distinguishes an empty history.
	if response == nil && after == before {
		return "", errors.New("browser: no previous page in this tab's history")
	}
	log.Info("went back", "from", before, "to", after)
	return after, nil
}

// ListTabs returns one-line descriptors of every open tab, marking the one the
// other operations act on.
func (b *Browser) ListTabs(ctx context.Context) ([]string, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if err := b.init(ctx); err != nil {
		return nil, err
	}
	pages := b.bctx.Pages()
	descriptors := make([]string, 0, len(pages))
	for index, page := range pages {
		marker := " "
		if page == b.page {
			marker = "*"
		}
		title, _ := page.Title()
		descriptors = append(descriptors, fmt.Sprintf("%s [%d] %s — %s", marker, index, title, page.URL()))
	}
	return descriptors, nil
}

// SelectTab points subsequent operations at the tab at index and returns its
// URL. Clicking a link that opens in a new tab leaves the harness holding the
// old page; this is how it moves across.
func (b *Browser) SelectTab(ctx context.Context, index int) (string, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if err := b.init(ctx); err != nil {
		return "", err
	}
	pages := b.bctx.Pages()
	if index < 0 || index >= len(pages) {
		return "", tabIndexError(index, len(pages))
	}
	b.page = pages[index]
	if err := b.page.BringToFront(); err != nil {
		log.Warn("bring tab to front failed", "index", index, "err", err)
	}
	log.Info("selected tab", "index", index, "url", b.page.URL())
	return b.page.URL(), nil
}

// CloseTab closes the tab at index.
//
// Closing the active tab would leave every later operation addressing a dead
// page, and closing the last one can take the browser down with it, so a
// replacement is chosen — or opened — before the close rather than after.
func (b *Browser) CloseTab(ctx context.Context, index int) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if err := b.init(ctx); err != nil {
		return err
	}
	pages := b.bctx.Pages()
	if index < 0 || index >= len(pages) {
		return tabIndexError(index, len(pages))
	}
	doomed := pages[index]
	if doomed == b.page {
		replacement, err := b.survivingTab(index, pages)
		if err != nil {
			return err
		}
		b.page = replacement
	}
	log.Info("closing tab", "index", index, "url", doomed.URL())
	return doomed.Close()
}

// survivingTab picks the page that becomes active once the page at index is
// closed, opening a blank one when index is the only tab left.
func (b *Browser) survivingTab(index int, pages []playwright.Page) (playwright.Page, error) {
	for position, page := range pages {
		if position != index {
			return page, nil
		}
	}
	page, err := b.bctx.NewPage()
	if err != nil {
		return nil, fmt.Errorf("browser: open replacement tab: %w", err)
	}
	return page, nil
}

func tabIndexError(index, open int) error {
	return fmt.Errorf("browser: tab index %d out of range (%d open; list them with browser_tabs)", index, open)
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
