// Package browser is `forge site audit --browser`'s render pass: it drives a
// Chrome or Chromium already installed on this machine through the DevTools
// protocol (chromedp), serializes each page's DOM after its JavaScript ran,
// and runs axe-core in the page when an axe-core script is at hand.
//
// forge never downloads or bundles a browser, or axe-core: a missing Chrome
// is an error naming the fix ([FindChrome]), and a missing axe-core leaves
// accessibility unscored (the CLI says so). It is its own package so the
// chromedp dependency stays confined to the one command that needs it.
package browser

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/chromedp/cdproto/network"
	"github.com/chromedp/chromedp"

	"github.com/reliant-labs/forge/pkg/siteaudit"
)

// ChromePathEnv names a Chrome or Chromium executable to use instead of
// searching PATH (the variable Lighthouse and Puppeteer read too).
const ChromePathEnv = "CHROME_PATH"

// chromeNames are the executables searched for on PATH, in order.
var chromeNames = []string{
	"google-chrome", "google-chrome-stable", "chromium", "chromium-browser",
	"chrome", "chrome-headless-shell", "headless_shell", "microsoft-edge",
}

// chromeAppPaths are where the platform installs Chrome outside PATH.
func chromeAppPaths() []string {
	switch runtime.GOOS {
	case "darwin":
		return []string{
			"/Applications/Google Chrome.app/Contents/MacOS/Google Chrome",
			"/Applications/Chromium.app/Contents/MacOS/Chromium",
		}
	case "windows":
		var out []string
		for _, env := range []string{"ProgramFiles", "ProgramFiles(x86)", "LocalAppData"} {
			if d := os.Getenv(env); d != "" {
				out = append(out, filepath.Join(d, "Google", "Chrome", "Application", "chrome.exe"))
			}
		}
		return out
	}
	return nil
}

// ErrNoChrome is returned (wrapped) by [FindChrome] when no browser is found.
var ErrNoChrome = errors.New("no Chrome or Chromium found")

// FindChrome returns the browser to drive: $CHROME_PATH, else the first of
// chromeNames on PATH, else the platform's install location.
func FindChrome() (string, error) {
	if p := strings.TrimSpace(os.Getenv(ChromePathEnv)); p != "" {
		if fi, err := os.Stat(p); err != nil || fi.IsDir() {
			return "", fmt.Errorf("%w: %s=%s is not an executable file", ErrNoChrome, ChromePathEnv, p)
		}
		return p, nil
	}
	for _, n := range chromeNames {
		if p, err := exec.LookPath(n); err == nil {
			return p, nil
		}
	}
	for _, p := range chromeAppPaths() {
		if fi, err := os.Stat(p); err == nil && !fi.IsDir() {
			return p, nil
		}
	}
	return "", fmt.Errorf("%w on PATH (looked for %s): --browser drives a browser you have installed and never downloads one. Install Google Chrome or Chromium, or set %s to its executable", ErrNoChrome, strings.Join(chromeNames, ", "), ChromePathEnv)
}

// Options configure a [Renderer].
type Options struct {
	// ExecPath is the browser executable ([FindChrome]).
	ExecPath string
	// AxeScript is axe-core's source (axe.min.js); empty runs no axe.
	AxeScript string
	// UserAgent is sent with every request the browser makes.
	UserAgent string
	// PageTimeout bounds one page's render (default 30s).
	PageTimeout time.Duration
	// Settle is how long a page is given after its load event for
	// client-side rendering (default 750ms).
	Settle time.Duration
	// Proxy is the network boundary every request the browser makes goes
	// through ([Proxy]); its Ports and AllowAddr decide what is reachable.
	Proxy ProxyOptions
}

// Renderer renders pages in one headless browser, one fresh tab per page.
type Renderer struct {
	o       Options
	browser context.Context
	cancel  func()
	proxy   *Proxy
}

// Start launches the browser behind its proxy (proxy.go): the browser
// reaches nothing but through it.
func Start(ctx context.Context, o Options) (*Renderer, error) {
	if o.PageTimeout <= 0 {
		o.PageTimeout = 30 * time.Second
	}
	if o.Settle <= 0 {
		o.Settle = 750 * time.Millisecond
	}
	proxy, err := StartProxy(o.Proxy)
	if err != nil {
		return nil, err
	}
	opts := append(chromedp.DefaultExecAllocatorOptions[:], chromedp.ExecPath(o.ExecPath))
	opts = append(opts, chromeProxyFlags(proxy.URL())...)
	if o.UserAgent != "" {
		opts = append(opts, chromedp.UserAgent(o.UserAgent))
	}
	// Chrome refuses to start as root with its sandbox on (containers, CI).
	if runtime.GOOS == "linux" && os.Geteuid() == 0 {
		opts = append(opts, chromedp.NoSandbox)
	}
	allocCtx, cancelAlloc := chromedp.NewExecAllocator(ctx, opts...)
	browserCtx, cancelBrowser := chromedp.NewContext(allocCtx)
	// Start the browser now, so a browser that cannot run fails here, once,
	// rather than on every page.
	if err := chromedp.Do(browserCtx); err != nil {
		cancelBrowser()
		cancelAlloc()
		proxy.Close()
		return nil, fmt.Errorf("start %s: %w", o.ExecPath, err)
	}
	return &Renderer{o: o, browser: browserCtx, proxy: proxy, cancel: func() { cancelBrowser(); cancelAlloc(); proxy.Close() }}, nil
}

// chromeProxyFlags send all of Chrome's traffic through the proxy at
// proxyURL: loopback included (Chrome bypasses a proxy for it by default),
// and no UDP that could go around it (WebRTC).
func chromeProxyFlags(proxyURL string) []chromedp.ExecAllocatorOption {
	return []chromedp.ExecAllocatorOption{
		chromedp.ProxyServer(proxyURL),
		chromedp.Flag("proxy-bypass-list", "<-loopback>"),
		chromedp.Flag("force-webrtc-ip-handling-policy", "disable_non_proxied_udp"),
		chromedp.Flag("webrtc-ip-handling-policy", "disable_non_proxied_udp"),
		chromedp.Flag("disable-quic", true),
	}
}

// Close stops the browser and its proxy.
func (r *Renderer) Close() { r.cancel() }

// Refused counts the requests the browser made that the proxy refused.
func (r *Renderer) Refused() int64 { return r.proxy.Refused() }

// blockedPatterns keep a render out of the site's own telemetry: the forge
// web runtime's OTLP and session-replay endpoints, so an audit never writes
// web vitals or a replay into the site's Speed data.
var blockedPatterns = []*network.BlockPattern{
	{URLPattern: "*://*:*/_otel*", Block: true},
	{URLPattern: "*://*:*/k/*", Block: true},
}

// serializeDOM is the page's DOM as a document, doctype included.
const serializeDOM = `(document.doctype ? new XMLSerializer().serializeToString(document.doctype) : "") + document.documentElement.outerHTML`

// runAxe runs axe-core and returns its result as JSON text (the four lists
// siteaudit reads).
const runAxe = `axe.run(document, {resultTypes: ["violations", "passes", "incomplete", "inapplicable"]}).then(function (r) {
  return JSON.stringify({violations: r.violations, passes: r.passes, incomplete: r.incomplete, inapplicable: r.inapplicable});
})`

// Render loads pageURL in a new tab and returns its rendered DOM, and axe's
// result when an axe script was given.
func (r *Renderer) Render(ctx context.Context, pageURL string) (string, *siteaudit.AxeResult, error) {
	tab, cancelTab := chromedp.NewContext(r.browser)
	defer cancelTab()
	tab, cancelTimeout := context.WithTimeout(tab, r.o.PageTimeout)
	defer cancelTimeout()
	stop := context.AfterFunc(ctx, cancelTab)
	defer stop()

	if _, err := chromedp.Call(tab, network.Enable, network.EnableParams{}); err == nil {
		// Best effort: a browser too old for URL patterns still renders.
		_, _ = chromedp.Call(tab, network.SetBlockedURLs, network.SetBlockedURLsParams{URLPatterns: blockedPatterns})
	}
	if err := chromedp.Do(tab, chromedp.Navigate(pageURL), chromedp.Sleep(r.o.Settle)); err != nil {
		return "", nil, err
	}
	html, err := chromedp.Run(tab, chromedp.Evaluate[string](serializeDOM))
	if err != nil {
		return "", nil, fmt.Errorf("serialize the DOM: %w", err)
	}
	if r.o.AxeScript == "" {
		return html, nil, nil
	}
	if _, err := chromedp.Run(tab, chromedp.Evaluate[chromedp.Void](r.o.AxeScript)); err != nil {
		return html, nil, fmt.Errorf("inject axe-core: %w", err)
	}
	raw, err := chromedp.Run(tab, chromedp.Evaluate[string](runAxe, chromedp.EvalAwaitPromise))
	if err != nil {
		return html, nil, fmt.Errorf("run axe-core: %w", err)
	}
	var axe siteaudit.AxeResult
	if err := json.Unmarshal([]byte(raw), &axe); err != nil {
		return html, nil, fmt.Errorf("read axe-core's result: %w", err)
	}
	return html, &axe, nil
}

// AxeScriptPaths are where axe-core's browser build is looked for under a
// directory: an npm install of axe-core.
var AxeScriptPaths = []string{filepath.Join("node_modules", "axe-core", "axe.min.js")}

// FindAxeScript returns the first axe-core script found in dirs or any of
// their parents, and "" when there is none.
func FindAxeScript(dirs ...string) string {
	seen := map[string]bool{}
	for _, d := range dirs {
		abs, err := filepath.Abs(d)
		if err != nil {
			continue
		}
		for dir := abs; ; dir = filepath.Dir(dir) {
			if !seen[dir] {
				seen[dir] = true
				for _, rel := range AxeScriptPaths {
					p := filepath.Join(dir, rel)
					if fi, err := os.Stat(p); err == nil && !fi.IsDir() {
						return p
					}
				}
			}
			if filepath.Dir(dir) == dir {
				break
			}
		}
	}
	return ""
}
