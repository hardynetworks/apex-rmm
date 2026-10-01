//go:build windows

package main

import (
	"encoding/json"
	"fmt"
	"html"
	"log"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"
)

// bridgeScript is injected into every page before it runs. It exposes window.apexNative to the
// Apex RMM dashboard, but only on the configured server's own origin (or our local setup page),
// so other sites (e.g. the Authentik login pages) never see it.
func bridgeScript(kind windowKind) string {
	k := "main"
	if kind == kindSession {
		k = "session"
	}
	return fmt.Sprintf(`(function () {
  var ORIGIN = %q, KIND = %q, VERSION = %q;
  var local = location.protocol === 'about:' || location.protocol === 'data:';
  if (!local && (!ORIGIN || location.origin !== ORIGIN)) return;
  if (!window.chrome || !window.chrome.webview) return;
  var seq = 1, pending = {};
  function call(fn, args) {
    return new Promise(function (resolve, reject) {
      var id = seq++;
      pending[id] = { resolve: resolve, reject: reject };
      window.chrome.webview.postMessage(JSON.stringify({ id: id, fn: fn, args: args || [] }));
    });
  }
  window.__apexReply = function (id, ok, value) {
    var p = pending[id]; if (!p) return; delete pending[id];
    ok ? p.resolve(value) : p.reject(new Error(value));
  };
  window.apexNative = {
    platform: 'windows', version: VERSION, kind: KIND,
    openSession: function (u) { return call('openSession', [String(u)]); },
    openExternal: function (u) { return call('openExternal', [String(u)]); },
    notify: function (title, body, link) { return call('notify', [String(title || ''), String(body || ''), String(link || '')]); },
    setCapture: function (on) { return call('setCapture', [!!on]); },
    getConfig: function () { return call('getConfig'); },
    testServer: function (u) { return call('testServer', [String(u)]); },
    setServer: function (u) { return call('setServer', [String(u)]); },
    setCaptureDefault: function (on) { return call('setCaptureDefault', [!!on]); },
    closeWindow: function () { return call('closeWindow'); }
  };
  if (local) return;

  // Remote-control links open in their own native window; other sites open in the default browser.
  var nativeOpen = window.open;
  window.open = function (u, name, features) {
    try {
      var abs = new URL(u, location.href);
      if (abs.origin === ORIGIN && /^\/devices\/[^\/]+\/remote$/.test(abs.pathname)) { call('openSession', [abs.href]); return null; }
      if (abs.origin !== ORIGIN) { call('openExternal', [abs.href]); return null; }
    } catch (e) {}
    return nativeOpen.apply(window, arguments);
  };
  document.addEventListener('click', function (e) {
    var el = e.target && e.target.closest ? e.target.closest('a[href]') : null;
    if (!el || e.defaultPrevented) return;
    var abs; try { abs = new URL(el.href, location.href); } catch (err) { return; }
    if (abs.origin !== ORIGIN || el.target === '_blank') {
      if (abs.origin === ORIGIN && /^\/devices\/[^\/]+\/remote$/.test(abs.pathname)) { e.preventDefault(); call('openSession', [abs.href]); return; }
      if (abs.origin !== ORIGIN) { e.preventDefault(); call('openExternal', [abs.href]); }
    }
  }, true);
  if (KIND === 'session') window.close = function () { call('closeWindow'); };

  // Keep the native window title in sync with the page.
  var last = '';
  setInterval(function () { if (document.title && document.title !== last) { last = document.title; call('setTitle', [last]); } }, 800);
})();`, origin(a.cfg.Server), k, version)
}

var sessionPath = regexp.MustCompile(`^/devices/[^/]+/remote$`)

type bridgeMsg struct {
	ID   int               `json:"id"`
	Fn   string            `json:"fn"`
	Args []json.RawMessage `json:"args"`
}

func (m *bridgeMsg) str(i int) string {
	var s string
	if i < len(m.Args) {
		_ = json.Unmarshal(m.Args[i], &s)
	}
	return s
}

func (m *bridgeMsg) boolean(i int) bool {
	var b bool
	if i < len(m.Args) {
		_ = json.Unmarshal(m.Args[i], &b)
	}
	return b
}

func (w *appWindow) reply(id int, value any, err error) {
	if id == 0 {
		return
	}
	var js string
	if err != nil {
		e, _ := json.Marshal(err.Error())
		js = fmt.Sprintf("window.__apexReply&&window.__apexReply(%d,false,%s)", id, e)
	} else {
		v, _ := json.Marshal(value)
		js = fmt.Sprintf("window.__apexReply&&window.__apexReply(%d,true,%s)", id, v)
	}
	a.dispatch(func() {
		if a.windows[w.hwnd] == w {
			w.eval(js)
		}
	})
}

// onMessage handles calls from window.apexNative. It runs on the UI thread inside a WebView2
// event, so anything that creates windows or blocks is deferred with a.dispatch / a goroutine.
func (w *appWindow) onMessage(raw string) {
	var m bridgeMsg
	if err := json.Unmarshal([]byte(raw), &m); err != nil {
		return
	}
	srv := origin(a.cfg.Server)
	switch m.Fn {
	case "openSession":
		u, err := url.Parse(m.str(0))
		if err != nil || srv == "" || u.Scheme+"://"+u.Host != srv || !sessionPath.MatchString(u.Path) {
			w.reply(m.ID, nil, errorf("not a remote-control address on this server"))
			return
		}
		target := u.String()
		a.dispatch(func() { a.openSession(target) })
		w.reply(m.ID, true, nil)

	case "openExternal":
		u, err := url.Parse(m.str(0))
		if err != nil || !(u.Scheme == "https" || u.Scheme == "http" || u.Scheme == "rustdesk" || u.Scheme == "mailto") {
			w.reply(m.ID, nil, errorf("only web, mailto and rustdesk links can be opened"))
			return
		}
		target := u.String()
		a.dispatch(func() { shellOpen(target, "") })
		w.reply(m.ID, true, nil)

	case "notify":
		title, body, link := m.str(0), m.str(1), m.str(2)
		if link != "" && !strings.HasPrefix(link, "/") {
			link = ""
		}
		a.notifyURL = link
		a.tray.balloon(title, body)
		w.reply(m.ID, true, nil)

	case "setCapture":
		w.capture = m.boolean(0)
		w.reply(m.ID, a.cfg.captureKeys(), nil)

	case "setCaptureDefault":
		on := m.boolean(0)
		a.cfg.CaptureKeys = &on
		_ = a.cfg.save()
		w.reply(m.ID, on, nil)

	case "getConfig":
		w.reply(m.ID, map[string]any{
			"server": a.cfg.Server, "version": version, "captureKeys": a.cfg.captureKeys(),
			"autostart": autostartEnabled(),
		}, nil)

	case "testServer", "setServer":
		if w.url != "" { // only the local setup page may change the server
			w.reply(m.ID, nil, errorf("not allowed here"))
			return
		}
	}
	switch m.Fn {
	case "testServer":
		go func(id int, in string) {
			s, err := normalizeServer(in)
			if err == nil {
				err = checkServer(s)
			}
			w.reply(id, s, err)
		}(m.ID, m.str(0))

	case "setServer":
		go func(id int, in string) {
			s, err := normalizeServer(in)
			if err == nil {
				err = checkServer(s)
			}
			if err != nil {
				w.reply(id, nil, err)
				return
			}
			a.dispatch(func() {
				a.cfg.Server = s
				if err := a.cfg.save(); err != nil {
					w.reply(id, nil, err)
					return
				}
				log.Printf("server set to %s", s)
				a.reopenAll()
			})
		}(m.ID, m.str(0))

	case "setTitle":
		t := strings.TrimSpace(m.str(0))
		if t == "" {
			t = appName
		}
		w.setTitle(t)

	case "closeWindow":
		pPostMessageW.Call(w.hwnd, wmClose, 0, 0)
	}
}

// reopenAll recreates the windows after the server changed (the bridge script captures the origin).
func (a *app) reopenAll() {
	for h, w := range a.windows {
		if w.kind == kindSession {
			pDestroyWindow.Call(h)
		}
	}
	if a.main != nil {
		old := a.main.hwnd
		a.main = nil
		delete(a.windows, old)
		pDestroyWindow.Call(old)
	}
	a.main = a.openMain(true)
}

// checkServer confirms the address is an Apex RMM server.
func checkServer(s string) error {
	c := &http.Client{Timeout: 10 * time.Second}
	resp, err := c.Get(s + "/healthz")
	if err != nil {
		return errorf("couldn't reach %s (%v)", s, shortErr(err))
	}
	defer resp.Body.Close()
	var h struct {
		OK bool `json:"ok"`
	}
	if resp.StatusCode != 200 || json.NewDecoder(resp.Body).Decode(&h) != nil || !h.OK {
		return errorf("%s answered, but it isn't an Apex RMM server", s)
	}
	return nil
}

func shortErr(err error) string {
	s := err.Error()
	if i := strings.LastIndex(s, ": "); i >= 0 {
		s = s[i+2:]
	}
	return s
}

// setupPage is shown on first start (and from the tray's "Change server…").
func setupPage(current string) string {
	return strings.Replace(setupHTML, "{{SERVER}}", html.EscapeString(current), 1)
}

const setupHTML = `<!doctype html><html><head><meta charset="utf-8"><title>Apex RMM</title>
<style>
:root{color-scheme:light dark;--bg:#f4f6fb;--card:#fff;--text:#0f172a;--muted:#64748b;--border:#d6dce8;--accent:#2563eb;--err:#dc2626}
@media (prefers-color-scheme:dark){:root{--bg:#0b1120;--card:#111a2e;--text:#e2e8f0;--muted:#94a3b8;--border:#24304a;--err:#f87171}}
*{box-sizing:border-box}body{margin:0;min-height:100vh;display:grid;place-items:center;background:var(--bg);color:var(--text);font:15px/1.5 "Segoe UI",system-ui,sans-serif}
.card{width:min(440px,92vw);background:var(--card);border:1px solid var(--border);border-radius:16px;padding:32px 28px;box-shadow:0 20px 50px rgba(15,23,42,.12)}
.brand{display:flex;flex-direction:column;align-items:center;gap:8px;margin-bottom:18px;text-align:center}
h1{font-size:21px;margin:0}p{margin:0;color:var(--muted)}label{display:block;font-weight:600;font-size:13.5px;margin:18px 0 6px}
input{width:100%;padding:11px 12px;font:inherit;border:1px solid var(--border);border-radius:10px;background:transparent;color:inherit}
input:focus{outline:2px solid var(--accent);outline-offset:1px;border-color:transparent}
button{margin-top:16px;width:100%;padding:11px;border:0;border-radius:10px;background:var(--accent);color:#fff;font:inherit;font-weight:600;cursor:pointer}
button[disabled]{opacity:.6;cursor:default}.hint{font-size:12.5px;margin-top:6px}.err{color:var(--err);font-size:13.5px;margin-top:10px;min-height:20px}
</style></head><body><form class="card" id="f">
<div class="brand"><svg viewBox="0 0 32 32" width="48" height="48"><rect width="32" height="32" rx="8" fill="#2563eb"/><path d="M8.5 24L16 8l7.5 16M11.6 18.5h8.8" fill="none" stroke="#fff" stroke-width="3.2" stroke-linecap="round" stroke-linejoin="round"/></svg>
<h1>Connect to your Apex RMM server</h1><p>Enter the address you use to open Apex RMM in a browser.</p></div>
<label for="s">Server address</label>
<input id="s" placeholder="rmm.example.com" value="{{SERVER}}" autocomplete="url" spellcheck="false" autofocus>
<div class="hint" style="color:var(--muted)">For example <b>rmm.example.com</b> or <b>http://192.168.1.10:8080</b></div>
<div class="err" id="e"></div>
<button id="b">Connect</button></form>
<script>
var f=document.getElementById('f'),s=document.getElementById('s'),e=document.getElementById('e'),b=document.getElementById('b');
f.addEventListener('submit',function(ev){ev.preventDefault();e.textContent='';b.disabled=true;b.textContent='Checking…';
 window.apexNative.setServer(s.value).catch(function(err){e.textContent=err.message;b.disabled=false;b.textContent='Connect';});});
</script></body></html>`
