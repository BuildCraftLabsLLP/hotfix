import SwiftUI
import AppKit

struct SettingsView: View {
    @EnvironmentObject var prefs: PreferencesManager
    @EnvironmentObject var monitor: ProcessMonitor
    @State private var newExclusion: String = ""
    @State private var showExclusionError: Bool = false
    @State private var logText: String = ""

    var body: some View {
        ZStack {
            Color(hex: "F6F4F0").ignoresSafeArea()

            ScrollView {
                VStack(spacing: 0) {
                    // Header
                    settingsHeader

                    // Content
                    VStack(spacing: 16) {
                        protectionSection
                        exclusionsSection
                        logsSection
                        aboutSection
                    }
                    .padding(.horizontal, 24)
                    .padding(.bottom, 28)
                }
            }
        }
        .frame(width: 640, height: 480)
        .background(Color(hex: "F6F4F0"))
    }

    // MARK: - Header
    @ViewBuilder
    private var settingsHeader: some View {
        HStack(spacing: 12) {
            ZStack {
                RoundedRectangle(cornerRadius: 10)
                    .fill(Color(hex: "141416"))
                    .frame(width: 40, height: 40)
                Image(systemName: "flame.fill")
                    .font(.system(size: 20, weight: .semibold))
                    .foregroundStyle(Color(hex: "C9461E"))
            }
            VStack(alignment: .leading, spacing: 1) {
                Text("Hotfix")
                    .font(.system(size: 20, weight: .semibold, design: .rounded))
                    .foregroundStyle(Color(hex: "141416"))
                Text("v\(UpdateChecker.currentVersion) · BuildCraft Labs")
                    .font(.system(size: 11, design: .rounded))
                    .foregroundStyle(Color(hex: "141416").opacity(0.45))
            }
            Spacer()
        }
        .padding(.horizontal, 24)
        .padding(.vertical, 18)
        .background(Color(hex: "EDEAE5"))
        .overlay(
            Rectangle()
                .frame(height: 1)
                .foregroundStyle(Color(hex: "141416").opacity(0.10)),
            alignment: .bottom
        )
    }

    // MARK: - Protection section
    @ViewBuilder
    private var protectionSection: some View {
        SettingsCard(title: "Protection") {
            VStack(spacing: 14) {
                // Master toggle
                SettingsRow {
                    VStack(alignment: .leading, spacing: 2) {
                        Text("Enable Hotfix")
                            .font(.system(size: 13, weight: .semibold, design: .rounded))
                            .foregroundStyle(Color(hex: "141416"))
                        Text("Monitor and terminate high-CPU processes")
                            .font(.system(size: 11, design: .rounded))
                            .foregroundStyle(Color(hex: "141416").opacity(0.45))
                    }
                } trailing: {
                    Toggle("", isOn: Binding(
                        get: { prefs.isEnabled },
                        set: { newVal in
                            prefs.isEnabled = newVal
                            if newVal {
                                monitor.start()
                            } else {
                                monitor.stop()
                            }
                        }
                    ))
                    .toggleStyle(.switch)
                    .tint(Color(hex: "C9461E"))
                    .labelsHidden()
                }

                BCDivider()

                // CPU threshold slider
                VStack(alignment: .leading, spacing: 8) {
                    HStack {
                        Text("CPU Threshold")
                            .font(.system(size: 13, weight: .semibold, design: .rounded))
                            .foregroundStyle(Color(hex: "141416"))
                        Spacer()
                        Text(String(format: "%.0f%%", prefs.cpuThreshold))
                            .font(.system(size: 13, weight: .bold, design: .rounded).monospacedDigit())
                            .foregroundStyle(Color(hex: "C9461E"))
                            .frame(width: 42, alignment: .trailing)
                    }
                    BrandSlider(value: $prefs.cpuThreshold, range: 50...95, step: 5)
                    Text("Kill processes using more than \(Int(prefs.cpuThreshold))% CPU sustained")
                        .font(.system(size: 11, design: .rounded))
                        .foregroundStyle(Color(hex: "141416").opacity(0.45))
                }

                BCDivider()

                // Kill duration slider
                VStack(alignment: .leading, spacing: 8) {
                    HStack {
                        Text("Kill After")
                            .font(.system(size: 13, weight: .semibold, design: .rounded))
                            .foregroundStyle(Color(hex: "141416"))
                        Spacer()
                        Text(durationLabel(prefs.killDuration))
                            .font(.system(size: 13, weight: .bold, design: .rounded).monospacedDigit())
                            .foregroundStyle(Color(hex: "C9461E"))
                            .frame(width: 64, alignment: .trailing)
                    }
                    BrandSlider(value: $prefs.killDuration, range: 30...300, step: 15)
                    Text("Terminate process after \(Int(prefs.killDuration)) seconds above threshold")
                        .font(.system(size: 11, design: .rounded))
                        .foregroundStyle(Color(hex: "141416").opacity(0.45))
                }

                BCDivider()

                // Kill on sleep toggle
                SettingsRow {
                    VStack(alignment: .leading, spacing: 2) {
                        Text("Kill on Sleep")
                            .font(.system(size: 13, weight: .semibold, design: .rounded))
                            .foregroundStyle(Color(hex: "141416"))
                        Text("Terminate hot processes when your Mac sleeps")
                            .font(.system(size: 11, design: .rounded))
                            .foregroundStyle(Color(hex: "141416").opacity(0.45))
                    }
                } trailing: {
                    Toggle("", isOn: $prefs.killOnSleep)
                        .toggleStyle(.switch)
                        .tint(Color(hex: "C9461E"))
                        .labelsHidden()
                }

                BCDivider()

                // Protect active app toggle
                SettingsRow {
                    VStack(alignment: .leading, spacing: 2) {
                        Text("Protect Active App")
                            .font(.system(size: 13, weight: .semibold, design: .rounded))
                            .foregroundStyle(Color(hex: "141416"))
                        Text("Never kill the app you're currently using")
                            .font(.system(size: 11, design: .rounded))
                            .foregroundStyle(Color(hex: "141416").opacity(0.45))
                    }
                } trailing: {
                    Toggle("", isOn: $prefs.protectActiveApp)
                        .toggleStyle(.switch)
                        .tint(Color(hex: "C9461E"))
                        .labelsHidden()
                }

                BCDivider()

                // Launch at login toggle
                SettingsRow {
                    VStack(alignment: .leading, spacing: 2) {
                        Text("Open at Login")
                            .font(.system(size: 13, weight: .semibold, design: .rounded))
                            .foregroundStyle(Color(hex: "141416"))
                        Text("Start Hotfix automatically when you log in to your Mac")
                            .font(.system(size: 11, design: .rounded))
                            .foregroundStyle(Color(hex: "141416").opacity(0.45))
                    }
                } trailing: {
                    Toggle("", isOn: Binding(
                        get: { prefs.launchAtLogin },
                        set: { prefs.setLaunchAtLogin($0) }
                    ))
                    .toggleStyle(.switch)
                    .tint(Color(hex: "C9461E"))
                    .labelsHidden()
                }
            }
        }
        .padding(.top, 20)
    }

    // MARK: - Exclusions section
    @ViewBuilder
    private var exclusionsSection: some View {
        SettingsCard(title: "Exclusions") {
            VStack(spacing: 10) {
                Text("These processes will never be terminated by Hotfix.")
                    .font(.system(size: 11, design: .rounded))
                    .foregroundStyle(Color(hex: "141416").opacity(0.45))
                    .frame(maxWidth: .infinity, alignment: .leading)

                // Add new exclusion
                HStack(spacing: 8) {
                    TextField("Process name (e.g. Xcode)", text: $newExclusion)
                        .textFieldStyle(.plain)
                        .font(.system(size: 12, design: .rounded))
                        .padding(.horizontal, 10)
                        .padding(.vertical, 7)
                        .background(
                            RoundedRectangle(cornerRadius: 7)
                                .fill(Color(hex: "F6F4F0"))
                                .overlay(
                                    RoundedRectangle(cornerRadius: 7)
                                        .strokeBorder(Color(hex: "141416").opacity(0.12), lineWidth: 1)
                                )
                        )
                        .onSubmit { addExclusion() }

                    Button(action: addExclusion) {
                        Text("Add")
                            .font(.system(size: 12, weight: .semibold, design: .rounded))
                            .foregroundStyle(.white)
                            .padding(.horizontal, 14)
                            .padding(.vertical, 7)
                            .background(
                                RoundedRectangle(cornerRadius: 7)
                                    .fill(Color(hex: "C9461E"))
                            )
                    }
                    .buttonStyle(.plain)
                }

                if showExclusionError {
                    Text("Process name cannot be empty.")
                        .font(.system(size: 10, design: .rounded))
                        .foregroundStyle(Color(hex: "C9461E"))
                        .frame(maxWidth: .infinity, alignment: .leading)
                }

                // List of exclusions
                if !prefs.whitelist.isEmpty {
                    VStack(spacing: 0) {
                        ForEach(prefs.whitelist, id: \.self) { name in
                            HStack {
                                Image(systemName: "shield.fill")
                                    .font(.system(size: 10))
                                    .foregroundStyle(Color(hex: "141416").opacity(0.30))
                                Text(name)
                                    .font(.system(size: 12, weight: .medium, design: .rounded))
                                    .foregroundStyle(Color(hex: "141416").opacity(0.80))
                                Spacer()
                                Button(action: { prefs.removeFromWhitelist(name) }) {
                                    Image(systemName: "xmark.circle.fill")
                                        .font(.system(size: 14))
                                        .foregroundStyle(Color(hex: "141416").opacity(0.25))
                                        .contentShape(Rectangle())
                                }
                                .buttonStyle(.plain)
                                .help("Remove \(name) from exclusions")
                            }
                            .padding(.horizontal, 10)
                            .padding(.vertical, 7)
                            .background(
                                Color(hex: "F6F4F0")
                                    .opacity(name == prefs.whitelist.last ? 1 : 0.01)
                            )

                            if name != prefs.whitelist.last {
                                Divider()
                                    .padding(.horizontal, 10)
                                    .overlay(Color(hex: "141416").opacity(0.07))
                            }
                        }
                    }
                    .background(
                        RoundedRectangle(cornerRadius: 8)
                            .fill(Color(hex: "F6F4F0"))
                            .overlay(
                                RoundedRectangle(cornerRadius: 8)
                                    .strokeBorder(Color(hex: "141416").opacity(0.10), lineWidth: 1)
                            )
                    )
                }
            }
        }
    }

    // MARK: - Logs section
    @ViewBuilder
    private var logsSection: some View {
        SettingsCard(title: "Logs") {
            VStack(spacing: 10) {
                HStack {
                    Text("Recent activity and process terminations.")
                        .font(.system(size: 11, design: .rounded))
                        .foregroundStyle(Color(hex: "141416").opacity(0.45))
                    Spacer()
                    Button(action: loadLogText) {
                        Text("Refresh")
                            .font(.system(size: 11, weight: .semibold, design: .rounded))
                            .foregroundStyle(Color(hex: "C9461E"))
                    }
                    .buttonStyle(.plain)
                    Text("·")
                        .foregroundStyle(Color(hex: "141416").opacity(0.25))
                    Button(action: openLogFile) {
                        Text("Open in Console")
                            .font(.system(size: 11, weight: .semibold, design: .rounded))
                            .foregroundStyle(Color(hex: "C9461E"))
                    }
                    .buttonStyle(.plain)
                }

                ScrollView {
                    Text(logText.isEmpty ? "No log entries yet." : logText)
                        .font(.system(size: 11, design: .monospaced))
                        .foregroundStyle(Color(hex: "141416").opacity(0.80))
                        .textSelection(.enabled)
                        .frame(maxWidth: .infinity, alignment: .leading)
                        .padding(10)
                }
                .frame(height: 160)
                .background(
                    RoundedRectangle(cornerRadius: 8)
                        .fill(Color(hex: "F6F4F0"))
                        .overlay(
                            RoundedRectangle(cornerRadius: 8)
                                .strokeBorder(Color(hex: "141416").opacity(0.10), lineWidth: 1)
                        )
                )
            }
        }
        .onAppear(perform: loadLogText)
    }

    // MARK: - About section
    @ViewBuilder
    private var aboutSection: some View {
        SettingsCard(title: "About") {
            VStack(spacing: 12) {
                HStack {
                    VStack(alignment: .leading, spacing: 2) {
                        Text("Hotfix v\(UpdateChecker.currentVersion)")
                            .font(.system(size: 13, weight: .semibold, design: .rounded))
                            .foregroundStyle(Color(hex: "141416"))
                        Text("macOS 13+ · Free · Open Source")
                            .font(.system(size: 11, design: .rounded))
                            .foregroundStyle(Color(hex: "141416").opacity(0.45))
                    }
                    Spacer()
                    Button(action: { UpdateChecker.shared.checkForUpdates(userInitiated: true) }) {
                        Text("Check for Updates")
                            .font(.system(size: 12, weight: .semibold, design: .rounded))
                            .foregroundStyle(Color(hex: "C9461E"))
                            .padding(.horizontal, 12)
                            .padding(.vertical, 6)
                            .background(
                                RoundedRectangle(cornerRadius: 7)
                                    .strokeBorder(Color(hex: "C9461E").opacity(0.40), lineWidth: 1)
                                    .background(
                                        RoundedRectangle(cornerRadius: 7)
                                            .fill(Color(hex: "C9461E").opacity(0.07))
                                    )
                            )
                    }
                    .buttonStyle(.plain)
                }

                BCDivider()

                HStack {
                    VStack(alignment: .leading, spacing: 2) {
                        Text("Built by BuildCraft Labs")
                            .font(.system(size: 12, weight: .semibold, design: .rounded))
                            .foregroundStyle(Color(hex: "141416").opacity(0.70))
                        Text("Crafting tools that respect your hardware.")
                            .font(.system(size: 11, design: .rounded))
                            .foregroundStyle(Color(hex: "141416").opacity(0.40))
                    }
                    Spacer()
                    HStack(spacing: 12) {
                        Link("GitHub", destination: URL(string: "https://github.com/buildcraftlabs/hotfix")!)
                            .font(.system(size: 11, weight: .semibold, design: .rounded))
                            .foregroundStyle(Color(hex: "C9461E"))
                        Text("·")
                            .foregroundStyle(Color(hex: "141416").opacity(0.25))
                        Link("Report Issue", destination: URL(string: "https://github.com/buildcraftlabs/hotfix/issues/new")!)
                            .font(.system(size: 11, weight: .semibold, design: .rounded))
                            .foregroundStyle(Color(hex: "C9461E"))
                    }
                }
            }
        }
        .padding(.bottom, 8)
    }

    // MARK: - Helpers
    private func durationLabel(_ seconds: Double) -> String {
        if seconds < 60 { return "\(Int(seconds))s" }
        let m = Int(seconds) / 60
        let s = Int(seconds) % 60
        return s == 0 ? "\(m)m" : "\(m)m \(s)s"
    }

    private func openLogFile() {
        guard let url = Log.shared.fileURL else { return }
        if !FileManager.default.fileExists(atPath: url.path) {
            FileManager.default.createFile(atPath: url.path, contents: nil)
        }
        NSWorkspace.shared.open(url)
    }

    /// Load the tail of the log file (last 200 lines) into the in-page viewer.
    ///
    /// Runs off the main thread and only reads the final chunk of the file — a
    /// synchronous whole-file read here froze the Settings window when the log had
    /// grown large and the machine was under load.
    private func loadLogText() {
        guard let url = Log.shared.fileURL else {
            logText = ""
            return
        }
        Task.detached(priority: .utility) {
            let tail = Self.tailOfLog(url, maxBytes: 64 * 1024, maxLines: 200)
            await MainActor.run { self.logText = tail }
        }
    }

    /// Read at most `maxBytes` from the end of the file and return its last
    /// `maxLines` lines. Bounded in both time and memory so it never blocks the UI.
    nonisolated private static func tailOfLog(_ url: URL, maxBytes: Int, maxLines: Int) -> String {
        guard let handle = try? FileHandle(forReadingFrom: url) else { return "" }
        defer { try? handle.close() }

        let size = (try? handle.seekToEnd()) ?? 0
        let start = size > UInt64(maxBytes) ? size - UInt64(maxBytes) : 0
        try? handle.seek(toOffset: start)

        guard let data = try? handle.readToEnd(),
              var text = String(data: data, encoding: .utf8) else { return "" }

        // If we started mid-file we may have sliced a line in half — drop it.
        if start > 0, let nl = text.firstIndex(of: "\n") {
            text = String(text[text.index(after: nl)...])
        }

        let lines = text.split(separator: "\n", omittingEmptySubsequences: true)
        return lines.suffix(maxLines).joined(separator: "\n")
    }

    private func addExclusion() {
        let trimmed = newExclusion.trimmingCharacters(in: .whitespacesAndNewlines)
        guard !trimmed.isEmpty else {
            showExclusionError = true
            return
        }
        showExclusionError = false
        prefs.addToWhitelist(trimmed)
        newExclusion = ""
    }
}

// MARK: - Reusable card
struct SettingsCard<Content: View>: View {
    let title: String
    @ViewBuilder let content: Content

    var body: some View {
        VStack(alignment: .leading, spacing: 12) {
            Text(title.uppercased())
                .font(.system(size: 10, weight: .bold, design: .rounded))
                .tracking(0.8)
                .foregroundStyle(Color(hex: "141416").opacity(0.40))

            VStack(spacing: 0) {
                content
                    .padding(14)
            }
            .background(
                RoundedRectangle(cornerRadius: 10)
                    .fill(Color(hex: "EDEAE5"))
                    .overlay(
                        RoundedRectangle(cornerRadius: 10)
                            .strokeBorder(Color(hex: "141416").opacity(0.08), lineWidth: 1)
                    )
            )
        }
    }
}

// MARK: - Settings row
struct SettingsRow<Leading: View, Trailing: View>: View {
    @ViewBuilder let leading: Leading
    @ViewBuilder let trailing: Trailing

    var body: some View {
        HStack(alignment: .center) {
            leading
            Spacer()
            trailing
        }
    }
}

// MARK: - Divider
struct BCDivider: View {
    var body: some View {
        Rectangle()
            .fill(Color(hex: "141416").opacity(0.08))
            .frame(height: 1)
    }
}
