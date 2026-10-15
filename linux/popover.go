//go:build linux

package main

// Makes the settings window behave like the Windows popover: no title bar,
// kept above other windows, no taskbar entry, placed in the screen corner by
// the tray, and dismissed when it loses focus. GTK is already loaded by
// libwebview; these calls go through purego. Under Wayland the compositor
// decides placement and stacking, so there the window simply opens centered.

import (
	"fmt"

	"github.com/ebitengine/purego"
)

// Popover size (same as Windows; the settings content is 640px wide), clamped
// to the work area at show time.
const (
	popWidth  = 700
	popHeight = 820
	popMargin = 12
)

// gdkRectangle mirrors GdkRectangle.
type gdkRectangle struct{ X, Y, Width, Height int32 }

var gtk struct {
	setDecorated   func(win uintptr, on int32)
	setKeepAbove   func(win uintptr, on int32)
	setSkipTaskbar func(win uintptr, on int32)
	setSkipPager   func(win uintptr, on int32)
	move           func(win uintptr, x, y int32)
	present        func(win uintptr)
	displayDefault func() uintptr
	primaryMonitor func(display uintptr) uintptr
	monitor        func(display uintptr, n int32) uintptr
	workarea       func(monitor uintptr, r *gdkRectangle)
	geometry       func(monitor uintptr, r *gdkRectangle)
	signalConnect  func(instance uintptr, signal string, handler, data, destroy uintptr, flags int32) uint64
}

// loadGTK resolves the GTK 3 / GDK / GObject calls used for the popover.
func loadGTK() (err error) {
	defer func() {
		if r := recover(); r != nil { // RegisterLibFunc panics on a missing symbol
			err = fmt.Errorf("gtk: %v", r)
		}
	}()
	var libs [3]uintptr
	for i, name := range []string{"libgtk-3.so.0", "libgdk-3.so.0", "libgobject-2.0.so.0"} {
		if libs[i], err = purego.Dlopen(name, purego.RTLD_NOW|purego.RTLD_GLOBAL); err != nil {
			return err
		}
	}
	gtkLib, gdkLib, gobject := libs[0], libs[1], libs[2]
	purego.RegisterLibFunc(&gtk.setDecorated, gtkLib, "gtk_window_set_decorated")
	purego.RegisterLibFunc(&gtk.setKeepAbove, gtkLib, "gtk_window_set_keep_above")
	purego.RegisterLibFunc(&gtk.setSkipTaskbar, gtkLib, "gtk_window_set_skip_taskbar_hint")
	purego.RegisterLibFunc(&gtk.setSkipPager, gtkLib, "gtk_window_set_skip_pager_hint")
	purego.RegisterLibFunc(&gtk.move, gtkLib, "gtk_window_move")
	purego.RegisterLibFunc(&gtk.present, gtkLib, "gtk_window_present")
	purego.RegisterLibFunc(&gtk.displayDefault, gdkLib, "gdk_display_get_default")
	purego.RegisterLibFunc(&gtk.primaryMonitor, gdkLib, "gdk_display_get_primary_monitor")
	purego.RegisterLibFunc(&gtk.monitor, gdkLib, "gdk_display_get_monitor")
	purego.RegisterLibFunc(&gtk.workarea, gdkLib, "gdk_monitor_get_workarea")
	purego.RegisterLibFunc(&gtk.geometry, gdkLib, "gdk_monitor_get_geometry")
	purego.RegisterLibFunc(&gtk.signalConnect, gobject, "g_signal_connect_data")
	return nil
}

// makePopover strips the window chrome, keeps it on top and out of the
// taskbar, sizes it, and moves it next to the tray.
func makePopover(w *webView, win uintptr) {
	width, height := popWidth, popHeight
	x, y, placed := 0, 0, false
	if display := gtk.displayDefault(); display != 0 {
		mon := gtk.primaryMonitor(display)
		if mon == 0 {
			mon = gtk.monitor(display, 0)
		}
		if mon != 0 {
			var wa, geo gdkRectangle
			gtk.workarea(mon, &wa)
			gtk.geometry(mon, &geo)
			x, y, width, height = popoverPlacement(wa, geo)
			placed = true
		}
	}
	gtk.setDecorated(win, 0)
	gtk.setKeepAbove(win, 1)
	gtk.setSkipTaskbar(win, 1)
	gtk.setSkipPager(win, 1)
	w.SetSize(width, height)
	if placed {
		gtk.move(win, int32(x), int32(y))
	}
}

// popoverPlacement returns the popover's position and size for a monitor's
// work area (screen minus panels) and full geometry: the bottom-right corner,
// where the Windows tray is — or the top-right corner when the panel is at the
// top of the screen (e.g. GNOME), so it still opens next to the tray.
func popoverPlacement(wa, geo gdkRectangle) (x, y, width, height int) {
	width, height = popWidth, popHeight
	if max := int(wa.Width) - 2*popMargin; width > max {
		width = max
	}
	if max := int(wa.Height) - 2*popMargin; height > max {
		height = max
	}
	x = int(wa.X) + int(wa.Width) - width - popMargin
	y = int(wa.Y) + int(wa.Height) - height - popMargin
	if wa.Y > geo.Y { // a panel takes the top edge
		y = int(wa.Y) + popMargin
	}
	return x, y, width, height
}

var (
	focusOutCB uintptr
	onFocusOut func()
)

// watchFocusOut calls fn on the UI thread whenever win loses focus.
func watchFocusOut(win uintptr, fn func()) {
	onFocusOut = fn
	if focusOutCB == 0 {
		// gboolean (*)(GtkWidget*, GdkEventFocus*, gpointer); FALSE = propagate.
		focusOutCB = purego.NewCallback(func(_, _, _ uintptr) uintptr {
			if onFocusOut != nil {
				onFocusOut()
			}
			return 0
		})
	}
	gtk.signalConnect(win, "focus-out-event", focusOutCB, 0, 0, 0)
}
