package browser

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/mxschmitt/playwright-go"
)

// connectLive attaches to a locally launched Chromium over CDP and returns a
// Browser whose connection fields are already populated, so init() short
// circuits and the tab methods run against a real browser. Skips when no
// Chromium or playwright driver is available.
func connectLive(t *testing.T) *Browser {
	t.Helper()
	chromePath, err := exec.LookPath("chromium")
	if err != nil {
		t.Skip("no chromium on PATH")
	}
	if err := playwright.Install(&playwright.RunOptions{SkipInstallBrowsers: true}); err != nil {
		t.Skipf("playwright driver unavailable: %v", err)
	}

	port := freePort(t)
	chrome := exec.Command(chromePath,
		"--headless=new", "--no-sandbox", "--disable-gpu", "--disable-extensions",
		"--no-first-run", "--no-default-browser-check",
		"--remote-debugging-port="+port, "--user-data-dir="+t.TempDir(), "about:blank")
	if err := chrome.Start(); err != nil {
		t.Skipf("cannot start chromium: %v", err)
	}
	t.Cleanup(func() {
		// A sandboxed chromium (a snap, say) can refuse our signal, and Wait
		// would then block for the rest of the run.
		_ = chrome.Process.Kill()
		reaped := make(chan struct{})
		go func() {
			_, _ = chrome.Process.Wait()
			close(reaped)
		}()
		select {
		case <-reaped:
		case <-time.After(5 * time.Second):
			t.Log("chromium did not exit; leaving it to the OS")
		}
	})

	pw, err := playwright.Run()
	if err != nil {
		t.Skipf("playwright run: %v", err)
	}
	t.Cleanup(func() { _ = pw.Stop() })

	endpoint := "http://127.0.0.1:" + port
	var connected playwright.Browser
	deadline := time.Now().Add(20 * time.Second)
	for {
		connected, err = pw.Chromium.ConnectOverCDP(endpoint)
		if err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Skipf("chromium never accepted CDP: %v", err)
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Cleanup(func() { _ = connected.Close() })

	bctx := connected.Contexts()[0]
	bctx.SetDefaultTimeout(float64(10 * time.Second / time.Millisecond))
	bctx.SetDefaultNavigationTimeout(float64(10 * time.Second / time.Millisecond))
	pages := bctx.Pages()
	if len(pages) == 0 {
		t.Skip("connected browser exposed no page")
	}
	return &Browser{browser: connected, bctx: bctx, page: pages[0]}
}

// freePort reserves a port and releases it, so consecutive tests do not attach
// to a chromium from an earlier one that has not exited yet. Reading the port
// back out of the profile directory would be tidier, but a sandboxed chromium
// writes it somewhere the test cannot see.
func freePort(t *testing.T) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Skipf("cannot reserve a port: %v", err)
	}
	defer listener.Close()
	_, port, err := net.SplitHostPort(listener.Addr().String())
	if err != nil {
		t.Skipf("cannot parse reserved address: %v", err)
	}
	return port
}

// serveTwoPages stands up a site with a link that opens in a new tab, which is
// the navigation failure the tab tools exist to recover from.
func serveTwoPages(t *testing.T) string {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		switch r.URL.Path {
		case "/second":
			_, _ = w.Write([]byte(`<title>Second</title><h1>Second page</h1>`))
		default:
			_, _ = w.Write([]byte(`<title>First</title><a id="out" href="/second" target="_blank">open</a>`))
		}
	}))
	t.Cleanup(server.Close)
	return server.URL
}

func TestLiveTabLifecycle(t *testing.T) {
	browser := connectLive(t)
	site := serveTwoPages(t)
	ctx := context.Background()

	if err := browser.Navigate(ctx, site); err != nil {
		t.Fatalf("navigate: %v", err)
	}

	// The click opens a second tab; the harness stays on the first.
	if err := browser.Click(ctx, "#out"); err != nil {
		t.Fatalf("click: %v", err)
	}
	waitForTabs(t, browser, 2)

	tabs, err := browser.ListTabs(ctx)
	if err != nil {
		t.Fatalf("list tabs: %v", err)
	}
	if len(tabs) != 2 {
		t.Fatalf("want 2 tabs, got %d: %v", len(tabs), tabs)
	}
	if !strings.HasPrefix(tabs[0], "*") {
		t.Errorf("tab 0 should be marked active, got %q", tabs[0])
	}
	if strings.HasPrefix(tabs[1], "*") {
		t.Errorf("tab 1 should not be marked active, got %q", tabs[1])
	}

	// Without selecting, content still comes from the original tab.
	before, err := browser.Content(ctx)
	if err != nil {
		t.Fatalf("content: %v", err)
	}
	if strings.Contains(before, "Second page") {
		t.Fatalf("expected to still be on the first tab, got:\n%s", before)
	}

	if _, err := browser.SelectTab(ctx, 1); err != nil {
		t.Fatalf("select tab: %v", err)
	}
	after, err := browser.Content(ctx)
	if err != nil {
		t.Fatalf("content after select: %v", err)
	}
	if !strings.Contains(after, "Second page") {
		t.Fatalf("expected the second tab's content, got:\n%s", after)
	}
}

func TestLiveCloseActiveTabMovesOn(t *testing.T) {
	browser := connectLive(t)
	site := serveTwoPages(t)
	ctx := context.Background()

	if err := browser.Navigate(ctx, site); err != nil {
		t.Fatalf("navigate: %v", err)
	}
	if err := browser.Click(ctx, "#out"); err != nil {
		t.Fatalf("click: %v", err)
	}
	waitForTabs(t, browser, 2)

	if _, err := browser.SelectTab(ctx, 1); err != nil {
		t.Fatalf("select tab: %v", err)
	}
	// Closing the tab we are standing on must leave a usable page behind.
	if err := browser.CloseTab(ctx, 1); err != nil {
		t.Fatalf("close active tab: %v", err)
	}
	content, err := browser.Content(ctx)
	if err != nil {
		t.Fatalf("content after closing the active tab: %v", err)
	}
	if !strings.Contains(content, "First") {
		t.Errorf("expected to fall back to the first tab, got:\n%s", content)
	}
}

func TestLiveCloseLastTabOpensReplacement(t *testing.T) {
	browser := connectLive(t)
	ctx := context.Background()

	tabs, err := browser.ListTabs(ctx)
	if err != nil {
		t.Fatalf("list tabs: %v", err)
	}
	for len(tabs) > 1 {
		if err := browser.CloseTab(ctx, len(tabs)-1); err != nil {
			t.Fatalf("close tab: %v", err)
		}
		if tabs, err = browser.ListTabs(ctx); err != nil {
			t.Fatalf("list tabs: %v", err)
		}
	}

	if err := browser.CloseTab(ctx, 0); err != nil {
		t.Fatalf("close last tab: %v", err)
	}
	tabs, err = browser.ListTabs(ctx)
	if err != nil {
		t.Fatalf("list tabs after closing the last: %v", err)
	}
	if len(tabs) != 1 {
		t.Fatalf("want a replacement tab, got %d: %v", len(tabs), tabs)
	}
	if _, err := browser.Eval(ctx, "() => 1 + 1"); err != nil {
		t.Errorf("replacement tab is not usable: %v", err)
	}
}

func TestLiveBack(t *testing.T) {
	browser := connectLive(t)
	site := serveTwoPages(t)
	ctx := context.Background()

	if _, err := browser.Back(ctx); err == nil {
		t.Error("want an error going back with no history, got nil")
	}
	if err := browser.Navigate(ctx, site); err != nil {
		t.Fatalf("navigate: %v", err)
	}
	if err := browser.Navigate(ctx, site+"/second"); err != nil {
		t.Fatalf("navigate: %v", err)
	}
	landed, err := browser.Back(ctx)
	if err != nil {
		t.Fatalf("back: %v", err)
	}
	if strings.HasSuffix(landed, "/second") {
		t.Errorf("back stayed on %q", landed)
	}
}

func TestLiveTabIndexOutOfRange(t *testing.T) {
	browser := connectLive(t)
	ctx := context.Background()

	if _, err := browser.SelectTab(ctx, 99); err == nil {
		t.Error("want an error selecting tab 99, got nil")
	}
	if err := browser.CloseTab(ctx, -1); err == nil {
		t.Error("want an error closing tab -1, got nil")
	}
}

func waitForTabs(t *testing.T, browser *Browser, want int) {
	t.Helper()
	for attempt := 0; attempt < 50; attempt++ {
		tabs, err := browser.ListTabs(context.Background())
		if err != nil {
			t.Fatalf("list tabs: %v", err)
		}
		if len(tabs) >= want {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("only saw fewer than %d tabs", want)
}
