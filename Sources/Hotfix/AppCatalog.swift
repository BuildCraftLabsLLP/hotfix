import Foundation
import AppKit

// The app list behind the Exclusions dropdown in Settings: the apps running
// right now (busiest first, with CPU use) followed by every other installed
// app. Mirrors the Windows/Linux dropdown (apps.go / installed.go there).
//
// Exclusions match a process's executable name exactly (see
// ProcessMonitor.processResults), so every entry's `name` is the executable
// that actually runs — e.g. "Electron" for Visual Studio Code — with the app's
// own name as its `label`.

/// One row of the dropdown.
struct AppEntry: Identifiable, Equatable {
    var id: String { name }
    /// What gets excluded: the process's executable name.
    let name: String
    /// The app's own name, when known ("Visual Studio Code").
    let label: String?
    /// CPU percent (of one core), summed over the app's processes.
    let cpu: Double
    let running: Bool
}

/// An installed .app bundle.
struct InstalledApp: Equatable {
    let name: String   // CFBundleExecutable
    let label: String  // display name
}

/// A running app with a Dock presence (NSWorkspace), by PID.
struct RunningGUIApp: Equatable {
    let pid: Int32
    let name: String   // executable name
    let label: String?
}

enum AppCatalog {
    /// CPU use (percent) at which a running process with no app behind it
    /// still makes the list. Idle helpers would bury the apps people look for
    /// (they can still be excluded by typing their name).
    static let minListedCPU = 0.5

    static let hotfixBundleID = "com.buildcraftlabs.hotfix"

    static var defaultAppDirectories: [URL] {
        var dirs = ["/Applications", "/Applications/Utilities",
                    "/System/Applications", "/System/Applications/Utilities"]
            .map { URL(fileURLWithPath: $0, isDirectory: true) }
        dirs.append(FileManager.default.homeDirectoryForCurrentUser
            .appendingPathComponent("Applications", isDirectory: true))
        return dirs
    }

    // MARK: - Installed apps

    /// The .app bundles in `directories` (and one level of subfolders, e.g.
    /// "/Applications/Adobe Photoshop 2024/"), sorted by label. Bundles
    /// without an executable, Hotfix itself and `excluded` names are left out;
    /// the first bundle seen for an executable name wins.
    static func installedApps(in directories: [URL] = defaultAppDirectories,
                              excluding excluded: Set<String> = []) -> [InstalledApp] {
        let fm = FileManager.default
        var seen = Set<String>()
        var apps: [InstalledApp] = []

        func visit(_ dir: URL, depth: Int) {
            guard let items = try? fm.contentsOfDirectory(
                at: dir, includingPropertiesForKeys: [.isDirectoryKey], options: [.skipsHiddenFiles]
            ) else { return }
            for item in items {
                if item.pathExtension.lowercased() == "app" {
                    if let app = installedApp(at: item),
                       !excluded.contains(app.name),
                       seen.insert(app.name).inserted {
                        apps.append(app)
                    }
                } else if depth > 0,
                          (try? item.resourceValues(forKeys: [.isDirectoryKey]))?.isDirectory == true {
                    visit(item, depth: depth - 1)
                }
            }
        }

        for dir in directories {
            visit(dir, depth: 1)
        }
        return apps.sorted { $0.label.localizedCaseInsensitiveCompare($1.label) == .orderedAscending }
    }

    /// Reads a bundle's Info.plist: its executable and display name.
    static func installedApp(at bundle: URL) -> InstalledApp? {
        let plist = bundle.appendingPathComponent("Contents/Info.plist")
        guard let data = try? Data(contentsOf: plist),
              let info = (try? PropertyListSerialization.propertyList(from: data, format: nil)) as? [String: Any],
              let exe = info["CFBundleExecutable"] as? String, !exe.isEmpty else {
            return nil
        }
        if (info["CFBundleIdentifier"] as? String) == hotfixBundleID {
            return nil
        }
        let fallback = bundle.deletingPathExtension().lastPathComponent
        let label = [info["CFBundleDisplayName"] as? String, info["CFBundleName"] as? String]
            .compactMap { $0 }
            .first { !$0.isEmpty } ?? fallback
        return InstalledApp(name: exe, label: label)
    }

    // MARK: - Merging

    /// Builds the dropdown: running entries (GUI apps, plus any other process
    /// using CPU), busiest first, then the installed apps that aren't running,
    /// by label. A running app takes its label from NSWorkspace or, failing
    /// that, from its installed bundle. `ps` rows are ProcessMonitor.runPS
    /// output; PIDs under 100 are system processes and skipped, as in the
    /// monitor.
    static func merge(gui: [RunningGUIApp],
                      ps: [(pid: Int32, cpu: Double, name: String)],
                      installed: [InstalledApp],
                      excluded: Set<String>) -> [AppEntry] {
        var installedLabel: [String: String] = [:]
        for app in installed where installedLabel[app.name] == nil {
            installedLabel[app.name] = app.label
        }

        var cpu: [String: Double] = [:]
        var label: [String: String] = [:]
        var order: [String] = []
        func note(_ name: String) {
            if cpu[name] == nil {
                cpu[name] = 0
                order.append(name)
            }
        }

        let psCPU = Dictionary(ps.map { ($0.pid, $0.cpu) }, uniquingKeysWith: +)
        let guiPIDs = Set(gui.map(\.pid))
        let guiNames = Set(gui.map(\.name))

        for app in gui where !excluded.contains(app.name) {
            note(app.name)
            cpu[app.name, default: 0] += psCPU[app.pid] ?? 0
            if label[app.name] == nil, let l = app.label ?? installedLabel[app.name] {
                label[app.name] = l
            }
        }
        for row in ps where row.pid >= 100 && !guiPIDs.contains(row.pid) && !excluded.contains(row.name) {
            note(row.name)
            cpu[row.name, default: 0] += row.cpu
            if label[row.name] == nil, let l = installedLabel[row.name] {
                label[row.name] = l
            }
        }

        var running: [AppEntry] = order.compactMap { name in
            let c = cpu[name] ?? 0
            guard guiNames.contains(name) || label[name] != nil || c >= minListedCPU else { return nil }
            return AppEntry(name: name, label: label[name], cpu: c, running: true)
        }
        running.sort {
            if $0.cpu != $1.cpu { return $0.cpu > $1.cpu }
            return $0.name.localizedCaseInsensitiveCompare($1.name) == .orderedAscending
        }

        var listed = Set(running.map(\.name))
        var entries = running
        for app in installed where !excluded.contains(app.name) && listed.insert(app.name).inserted {
            entries.append(AppEntry(name: app.name, label: app.label, cpu: 0, running: false))
        }
        return entries
    }

    /// The entries whose process name or label contains `query`
    /// (case-insensitive); all of them for an empty query.
    static func filter(_ entries: [AppEntry], query: String) -> [AppEntry] {
        let q = query.trimmingCharacters(in: .whitespacesAndNewlines).lowercased()
        guard !q.isEmpty else { return entries }
        return entries.filter {
            $0.name.lowercased().contains(q) || ($0.label?.lowercased().contains(q) ?? false)
        }
    }
}

/// Loads the dropdown's list off the main thread when it's opened; the Settings
/// view observes `entries` (nil while the first load is running).
@MainActor
final class AppCatalogModel: ObservableObject {
    @Published private(set) var entries: [AppEntry]?
    private var loading = false
    private var loadedAt = Date.distantPast

    /// Nonisolated so a view can create it as a @StateObject default value.
    nonisolated init() {}

    /// Reloads unless a load is running or the list is under 5 seconds old.
    func refresh() {
        guard !loading, Date().timeIntervalSince(loadedAt) > 5 else { return }
        loading = true

        var excluded = ProcessMonitor.shared.safetyExclusions
        if let selfName = Bundle.main.executableURL?.lastPathComponent {
            excluded.insert(selfName)
        }
        let gui: [RunningGUIApp] = NSWorkspace.shared.runningApplications.compactMap { app in
            guard app.activationPolicy == .regular,
                  let exe = app.executableURL?.lastPathComponent else { return nil }
            return RunningGUIApp(pid: app.processIdentifier, name: exe, label: app.localizedName)
        }

        Task.detached(priority: .userInitiated) { [excluded, gui] in
            let ps = ProcessMonitor.runPS()
            let installed = AppCatalog.installedApps(excluding: excluded)
            let merged = AppCatalog.merge(gui: gui, ps: ps, installed: installed, excluded: excluded)
            await MainActor.run {
                self.entries = merged
                self.loading = false
                self.loadedAt = Date()
            }
        }
    }
}
