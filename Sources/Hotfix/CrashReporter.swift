import Foundation
import AppKit
import Darwin

// Crash reporting for macOS — mirrors the Windows crashreport.go behavior.
//
// A crash (an uncaught NSException, or a fatal signal such as a Swift force-
// unwrap trap / segfault) is captured to a marker file. On the next launch the
// app opens a pre-filled GitHub "New Issue" page so the user can submit it with
// one click. A pre-filled URL is used instead of the GitHub API on purpose:
// filing issues programmatically would require an embedded auth token. This
// keeps reporting tokenless and lets the user review before sending.

enum CrashReporter {
    static let issueBaseURL = "https://github.com/buildcraftlabs/hotfix/issues/new"

    /// Marker file, kept beside the log (~/Library/Logs/Hotfix/lastcrash.txt).
    static var crashFileURL: URL? {
        Log.defaultDirectory?.appendingPathComponent("lastcrash.txt")
    }

    /// Arms the crash handlers for this session. Call once at launch, after
    /// `reportPending()` has consumed any crash from the previous run.
    static func install() {
        if let path = crashFileURL?.path {
            // strdup'd once and intentionally leaked: the signal handler must
            // reach the path without allocating (async-signal-safe).
            crashMarkerPathC = strdup(path)
        }

        NSSetUncaughtExceptionHandler { exception in
            let stack = exception.callStackSymbols.joined(separator: "\n")
            CrashReporter.record("Uncaught exception: \(exception.name.rawValue): \(exception.reason ?? "")\n\n\(stack)")
        }

        for sig in [SIGABRT, SIGILL, SIGSEGV, SIGFPE, SIGBUS, SIGTRAP] {
            _ = signal(sig, { signo in crashHandleSignal(signo) })
        }
    }

    /// Persists a crash message (used by the NSException handler; the signal
    /// handler writes its own marker via async-signal-safe primitives).
    static func record(_ message: String) {
        logf("CRASH \(message.prefix(200))")
        guard let url = crashFileURL else { return }
        let stamp = ISO8601DateFormatter().string(from: Date())
        let entry = "[\(stamp)] \(message)\n"
        try? FileManager.default.createDirectory(at: url.deletingLastPathComponent(),
                                                 withIntermediateDirectories: true)
        try? entry.data(using: .utf8)?.write(to: url)
    }

    /// If a crash marker from a previous run exists, open a pre-filled issue and
    /// remove the marker so it is never reported twice.
    static func reportPending() {
        guard let url = crashFileURL,
              let data = try? Data(contentsOf: url), !data.isEmpty,
              let crash = String(data: data, encoding: .utf8) else { return }
        try? FileManager.default.removeItem(at: url)
        logf("crashreport: previous crash found — opening pre-filled issue")
        if let issue = buildIssueURL(crash: crash) {
            NSWorkspace.shared.open(issue)
        }
    }

    /// Architecture label for the report (mirrors GOARCH on Windows).
    static var archString: String {
        #if arch(arm64)
        return "arm64"
        #elseif arch(x86_64)
        return "x86_64"
        #else
        return "unknown"
        #endif
    }

    /// Convenience that fills in version/arch and the recent log tail.
    static func buildIssueURL(crash: String) -> URL? {
        let recent = Log.shared.fileURL.flatMap { try? String(contentsOf: $0, encoding: .utf8) }
        return buildIssueURL(crash: crash, version: UpdateChecker.currentVersion,
                             arch: archString, recentLog: recent)
    }

    /// Pure builder (no I/O) so it can be unit-tested.
    static func buildIssueURL(crash: String, version: String, arch: String, recentLog: String?) -> URL? {
        let title = "Crash report — Hotfix v\(version) (macOS/\(arch))"

        var body = "_Automated crash report — please review and remove anything sensitive before submitting._\n\n"
        body += "- **Version:** \(version)\n- **OS:** macOS (\(arch))\n\n"
        body += "### Crash / stack trace\n```\n\(truncate(crash, 3500))\n```\n"
        if let log = recentLog, !log.isEmpty {
            body += "\n### Recent log\n```\n\(truncate(tailLines(log, 40), 1500))\n```\n"
        }

        var comps = URLComponents(string: issueBaseURL)
        comps?.queryItems = [
            URLQueryItem(name: "title", value: title),
            URLQueryItem(name: "labels", value: "crash"),
            URLQueryItem(name: "body", value: body),
        ]
        return comps?.url
    }

    static func truncate(_ s: String, _ max: Int) -> String {
        s.count <= max ? s : String(s.prefix(max)) + "\n…(truncated)"
    }

    static func tailLines(_ s: String, _ n: Int) -> String {
        let trimmed = s.trimmingCharacters(in: CharacterSet(charactersIn: "\n"))
        let lines = trimmed.components(separatedBy: "\n")
        return lines.count > n ? lines.suffix(n).joined(separator: "\n") : lines.joined(separator: "\n")
    }
}

// MARK: - Async-signal-safe signal handling
//
// These live at file scope (not inside the enum) and avoid heap allocation so
// they are safe to run from a signal handler: a pre-strdup'd path, a
// pre-allocated backtrace buffer, and only open/write/backtrace_symbols_fd.

fileprivate var crashMarkerPathC: UnsafeMutablePointer<CChar>?
fileprivate var crashBacktrace = [UnsafeMutableRawPointer?](repeating: nil, count: 64)

fileprivate func crashWrite(_ fd: Int32, _ s: StaticString) {
    s.withUTF8Buffer { buf in _ = write(fd, buf.baseAddress, buf.count) }
}

fileprivate func crashHandleSignal(_ signo: Int32) {
    if let path = crashMarkerPathC {
        let fd = open(path, O_WRONLY | O_CREAT | O_TRUNC, 0o644)
        if fd >= 0 {
            crashWrite(fd, "Fatal signal received.\n\nBacktrace:\n")
            let n = backtrace(&crashBacktrace, Int32(crashBacktrace.count))
            backtrace_symbols_fd(&crashBacktrace, n, fd)
            close(fd)
        }
    }
    // Restore the default handler and re-raise so the OS still records the crash.
    _ = signal(signo, SIG_DFL)
    _ = raise(signo)
}
