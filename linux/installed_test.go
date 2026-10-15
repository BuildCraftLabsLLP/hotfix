//go:build linux

package main

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestSplitExec(t *testing.T) {
	cases := map[string][]string{
		`/usr/bin/foo %U`:                          {"/usr/bin/foo"},
		`foo --bar "a b" %f`:                       {"foo", "--bar", "a b"},
		`"/opt/My App/app" --x`:                    {"/opt/My App/app", "--x"},
		`sh -c "echo \\"hi\\""`:                    {"sh", "-c", `echo "hi"`}, // string escapes first, then quoting
		`/usr/bin/a\sb`:                            {"/usr/bin/a", "b"},       // \s is a plain space: it separates
		`env FOO=1 BAR=2 /usr/bin/baz %u`:          {"env", "FOO=1", "BAR=2", "/usr/bin/baz"},
		`flatpak run --command=gimp org.gimp.GIMP`: {"flatpak", "run", "--command=gimp", "org.gimp.GIMP"},
	}
	for in, want := range cases {
		if got := splitExec(in); !reflect.DeepEqual(got, want) {
			t.Errorf("splitExec(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestExecProgram(t *testing.T) {
	flatpakCmd := func(id string) string {
		if id == "org.mozilla.firefox" {
			return "firefox"
		}
		return ""
	}
	cases := []struct{ exec, tryExec, want string }{
		{"/usr/bin/code --unity-launch %F", "", "code"},
		{"env BAMF_DESKTOP_FILE_HINT=/x.desktop /snap/bin/spotify %U", "", "spotify"},
		{"/usr/bin/env GDK_BACKEND=x11 blender %f", "", "blender"},
		{"/usr/bin/flatpak run --branch=stable --arch=x86_64 --file-forwarding org.mozilla.firefox @@u %u @@", "", "firefox"},
		{"flatpak run --command=gimp-2.10 org.gimp.GIMP %U", "", "gimp-2.10"},
		{"flatpak run org.gnome.Calculator", "", "calculator"}, // no metadata: last ID part
		{"snap run vlc %U", "", "vlc"},
		{`sh -c "steam %U"`, "/usr/games/steam", "steam"},
		{`bash -c "something"`, "", ""},
		{"", "/usr/bin/xterm", "xterm"},
		{"", "", ""},
	}
	for _, c := range cases {
		if got := execProgram(c.exec, c.tryExec, flatpakCmd); got != c.want {
			t.Errorf("execProgram(%q, %q) = %q, want %q", c.exec, c.tryExec, got, c.want)
		}
	}
}

func TestParseDesktopEntry(t *testing.T) {
	entry := `[Desktop Entry]
Name=Visual Studio Code
Name[de]=Visual Studio Code (DE)
Exec=/usr/share/code/code --unity-launch %F
Type=Application

[Desktop Action new-empty-window]
Name=New Empty Window
Exec=/usr/share/code/code --new-window %F
`
	app, ok := parseDesktopEntry(strings.NewReader(entry), nil)
	if !ok || app.Name != "code" || app.Label != "Visual Studio Code" {
		t.Errorf("got %+v, %v", app, ok)
	}

	for name, body := range map[string]string{
		"hidden":   "[Desktop Entry]\nType=Application\nName=X\nExec=x\nNoDisplay=true\n",
		"deleted":  "[Desktop Entry]\nType=Application\nName=X\nExec=x\nHidden=true\n",
		"link":     "[Desktop Entry]\nType=Link\nName=X\nURL=https://example.com\n",
		"no name":  "[Desktop Entry]\nType=Application\nExec=x\n",
		"no exec":  "[Desktop Entry]\nType=Application\nName=X\n",
		"no group": "Type=Application\nName=X\nExec=x\n",
		"shell":    "[Desktop Entry]\nType=Application\nName=X\nExec=sh -c 'x'\n",
	} {
		if _, ok := parseDesktopEntry(strings.NewReader(body), nil); ok {
			t.Errorf("%s: entry should be skipped", name)
		}
	}
}

func writeDesktop(t *testing.T, dir, file, name, exec string, extra ...string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	body := "[Desktop Entry]\nType=Application\nName=" + name + "\nExec=" + exec + "\n" + strings.Join(extra, "\n") + "\n"
	if err := os.WriteFile(filepath.Join(dir, file), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestInstalledApps_ScansXDGDirs(t *testing.T) {
	root := t.TempDir()
	user := filepath.Join(root, "user")
	sys1 := filepath.Join(root, "sys1")
	sys2 := filepath.Join(root, "sys2")
	t.Setenv("HOME", filepath.Join(root, "home"))
	t.Setenv("XDG_DATA_HOME", user)
	t.Setenv("XDG_DATA_DIRS", sys1+":"+sys2)
	saved := systemAppBases
	systemAppBases = nil // don't pick up this machine's Flatpak/Snap apps
	t.Cleanup(func() { systemAppBases = saved })

	writeDesktop(t, filepath.Join(sys1, "applications"), "firefox.desktop", "Firefox Web Browser", "firefox %u")
	writeDesktop(t, filepath.Join(sys1, "applications"), "blender.desktop", "Blender", "blender %f")
	// The user's copy of blender.desktop overrides the system one.
	writeDesktop(t, filepath.Join(user, "applications"), "blender.desktop", "Blender (mine)", "/opt/blender/blender")
	writeDesktop(t, filepath.Join(sys2, "applications", "kde4"), "kate.desktop", "Kate", "kate -b %U") // subdir
	writeDesktop(t, filepath.Join(sys2, "applications"), "hidden.desktop", "Hidden", "hidden", "NoDisplay=true")
	writeDesktop(t, filepath.Join(sys2, "applications"), "xorg.desktop", "X server", "Xorg")          // protected
	writeDesktop(t, filepath.Join(sys2, "applications"), "ff2.desktop", "Firefox (again)", "firefox") // same program
	writeDesktop(t, filepath.Join(sys2, "applications"), "hotfix.desktop", "Hotfix", "hotfix")        // itself

	got := installedApps()
	var labels []string
	for _, a := range got {
		labels = append(labels, a.Label+"="+a.Name)
	}
	want := "Blender (mine)=blender,Firefox Web Browser=firefox,Kate=kate"
	if strings.Join(labels, ",") != want {
		t.Errorf("got %s\nwant %s", strings.Join(labels, ","), want)
	}
}

func TestMergeApps(t *testing.T) {
	running := []runningApp{{Name: "firefox", CPU: 40}, {Name: "bash", CPU: 1}, {Name: "(sd-pam)", CPU: 0}}
	installed := []installedApp{
		{Name: "blender", Label: "Blender"},
		{Name: "Firefox", Label: "Firefox Web Browser"}, // running: labels it, not listed again
		{Name: "gimp", Label: "GNU Image Manipulation Program"},
	}
	got := mergeApps(running, installed)
	want := []appEntry{
		{Name: "firefox", Label: "Firefox Web Browser", CPU: 40, Running: true},
		{Name: "bash", CPU: 1, Running: true},
		{Name: "blender", Label: "Blender"},
		{Name: "gimp", Label: "GNU Image Manipulation Program"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got  %+v\nwant %+v", got, want)
	}
}
