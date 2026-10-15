CI builds `libwebview.so` (the [webview](https://github.com/webview/webview) C
library, pinned version, against WebKitGTK 4.1) into this directory before
`go build`, which embeds it into the Hotfix binary for the settings window.
See the `Build webview library` step in `.github/workflows/build.yml`.

The library is not committed. A build without it still works; only the
settings window is unavailable (the tray then opens `config.json` instead).
