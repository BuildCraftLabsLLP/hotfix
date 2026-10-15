//go:build linux

package main

// A minimal binding to the webview C library (github.com/webview/webview —
// the same library the Windows app's go-webview2 is a port of), loaded at
// runtime with purego. The app needs no C toolchain to build, and the tray
// keeps working on systems without WebKitGTK: only the settings window process
// ever loads the library. See webviewlib.go for where libwebview.so comes from.

import (
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"unsafe"

	"github.com/ebitengine/purego"
)

// webview_hint_t
const webviewHintNone int32 = 0

// Native entry points, resolved by loadWebviewLib. Signatures follow
// webview.h (0.12.0); the webview_error_t results are ignored.
var wv struct {
	create    func(debug int32, window uintptr) uintptr
	destroy   func(w uintptr) int32
	run       func(w uintptr) int32
	terminate func(w uintptr) int32
	dispatch  func(w uintptr, fn uintptr, arg uintptr) int32
	getWindow func(w uintptr) uintptr
	setTitle  func(w uintptr, title string) int32
	setSize   func(w uintptr, width, height, hints int32) int32
	setHTML   func(w uintptr, html string) int32
	initJS    func(w uintptr, js string) int32
	bind      func(w uintptr, name string, fn uintptr, arg uintptr) int32
	ret       func(w uintptr, id string, status int32, result string) int32
}

// C callbacks into Go, created once (purego callbacks are never freed).
var dispatchCB, bindCB uintptr

// loadWebviewLib loads libwebview.so from path and resolves its API. It fails
// (rather than panicking) when the library or WebKitGTK is missing.
func loadWebviewLib(path string) (err error) {
	defer func() {
		// RegisterLibFunc panics on a missing symbol.
		if r := recover(); r != nil {
			err = fmt.Errorf("webview: %v", r)
		}
	}()
	h, err := purego.Dlopen(path, purego.RTLD_NOW|purego.RTLD_GLOBAL)
	if err != nil {
		return err
	}
	purego.RegisterLibFunc(&wv.create, h, "webview_create")
	purego.RegisterLibFunc(&wv.destroy, h, "webview_destroy")
	purego.RegisterLibFunc(&wv.run, h, "webview_run")
	purego.RegisterLibFunc(&wv.terminate, h, "webview_terminate")
	purego.RegisterLibFunc(&wv.dispatch, h, "webview_dispatch")
	purego.RegisterLibFunc(&wv.getWindow, h, "webview_get_window")
	purego.RegisterLibFunc(&wv.setTitle, h, "webview_set_title")
	purego.RegisterLibFunc(&wv.setSize, h, "webview_set_size")
	purego.RegisterLibFunc(&wv.setHTML, h, "webview_set_html")
	purego.RegisterLibFunc(&wv.initJS, h, "webview_init")
	purego.RegisterLibFunc(&wv.bind, h, "webview_bind")
	purego.RegisterLibFunc(&wv.ret, h, "webview_return")
	dispatchCB = purego.NewCallback(onDispatch)
	bindCB = purego.NewCallback(onBind)
	return nil
}

// bindFunc handles a call from JavaScript: args are the JSON-encoded call
// arguments; the result is JSON-encoded back to the page's promise.
type bindFunc func(args []json.RawMessage) (any, error)

type binding struct {
	w  uintptr
	fn bindFunc
}

var (
	cbMu         sync.Mutex
	dispatchFns  = map[uintptr]func(){}
	nextDispatch uintptr
	bindings     = map[uintptr]binding{}
	nextBinding  uintptr
)

// webView is one webview window. All methods except Terminate and Dispatch
// must be called on the thread that created it.
type webView struct{ h uintptr }

func newWebView(debug bool) (*webView, error) {
	var d int32
	if debug {
		d = 1
	}
	h := wv.create(d, 0)
	if h == 0 {
		return nil, errors.New("webview: could not create a window (no display?)")
	}
	return &webView{h: h}, nil
}

func (w *webView) Run()                { wv.run(w.h) }
func (w *webView) Terminate()          { wv.terminate(w.h) }
func (w *webView) Destroy()            { wv.destroy(w.h) }
func (w *webView) Window() uintptr     { return wv.getWindow(w.h) }
func (w *webView) SetTitle(t string)   { wv.setTitle(w.h, t) }
func (w *webView) SetHTML(html string) { wv.setHTML(w.h, html) }
func (w *webView) Init(js string)      { wv.initJS(w.h, js) }
func (w *webView) SetSize(width, height int) {
	wv.setSize(w.h, int32(width), int32(height), webviewHintNone)
}

// Dispatch runs fn on the UI thread. Safe from any goroutine.
func (w *webView) Dispatch(fn func()) {
	cbMu.Lock()
	id := nextDispatch
	nextDispatch++
	dispatchFns[id] = fn
	cbMu.Unlock()
	wv.dispatch(w.h, dispatchCB, id)
}

// Bind exposes fn to the page as a global async function called name.
func (w *webView) Bind(name string, fn bindFunc) {
	cbMu.Lock()
	id := nextBinding
	nextBinding++
	bindings[id] = binding{w: w.h, fn: fn}
	cbMu.Unlock()
	wv.bind(w.h, name, bindCB, id)
}

func onDispatch(_, arg uintptr) uintptr {
	cbMu.Lock()
	fn := dispatchFns[arg]
	delete(dispatchFns, arg)
	cbMu.Unlock()
	if fn != nil {
		fn()
	}
	return 0
}

// onBind runs on the UI thread for every call to a bound function. The id and
// request strings are only valid during the callback, so they're copied; the
// function then runs on its own goroutine — so slow work (sampling CPU for the
// running-apps list) never freezes the window — and replies through
// webview_return, which is thread-safe. A failure rejects the page's promise
// with {message}, which the settings page displays.
func onBind(idPtr, reqPtr, arg uintptr) uintptr {
	id, req := goString(idPtr), goString(reqPtr)
	cbMu.Lock()
	b, ok := bindings[arg]
	cbMu.Unlock()
	if !ok {
		return 0
	}
	go reply(b, id, req)
	return 0
}

// reply runs a bound function and sends its result back to the page.
func reply(b binding, id, req string) {
	value, err := func() (v any, err error) {
		defer func() {
			if r := recover(); r != nil {
				err = fmt.Errorf("internal error: %v", r)
			}
		}()
		var args []json.RawMessage
		if err := json.Unmarshal([]byte(req), &args); err != nil {
			return nil, err
		}
		return b.fn(args)
	}()

	status, result := int32(0), "null"
	if err == nil {
		data, merr := json.Marshal(value)
		if merr == nil {
			result = string(data)
		} else {
			err = merr
		}
	}
	if err != nil {
		data, _ := json.Marshal(map[string]string{"message": err.Error()})
		status, result = 1, string(data)
	}
	wv.ret(b.w, id, status, result)
}

// goString copies a NUL-terminated C string.
func goString(p uintptr) string {
	if p == 0 {
		return ""
	}
	ptr := *(*unsafe.Pointer)(unsafe.Pointer(&p)) // uintptr → pointer without a vet warning
	n := 0
	for *(*byte)(unsafe.Add(ptr, n)) != 0 {
		n++
	}
	return string(unsafe.Slice((*byte)(ptr), n))
}
