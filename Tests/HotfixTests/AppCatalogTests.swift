import XCTest
@testable import Hotfix

final class AppCatalogTests: XCTestCase {

    private var root: URL!

    override func setUpWithError() throws {
        root = FileManager.default.temporaryDirectory
            .appendingPathComponent("AppCatalogTests-\(UUID().uuidString)", isDirectory: true)
        try FileManager.default.createDirectory(at: root, withIntermediateDirectories: true)
    }

    override func tearDownWithError() throws {
        try? FileManager.default.removeItem(at: root)
    }

    /// Creates `<dir>/<bundle>/Contents/Info.plist` with `info`.
    private func makeApp(in dir: URL, _ bundle: String, _ info: [String: Any]) throws {
        let contents = dir.appendingPathComponent(bundle).appendingPathComponent("Contents")
        try FileManager.default.createDirectory(at: contents, withIntermediateDirectories: true)
        let data = try PropertyListSerialization.data(fromPropertyList: info, format: .binary, options: 0)
        try data.write(to: contents.appendingPathComponent("Info.plist"))
    }

    // MARK: - Installed apps

    func testInstalledAppsReadsBundles() throws {
        let apps = root.appendingPathComponent("Applications")
        let user = root.appendingPathComponent("UserApplications")
        try makeApp(in: apps, "Visual Studio Code.app",
                    ["CFBundleExecutable": "Electron", "CFBundleName": "Code",
                     "CFBundleDisplayName": "Visual Studio Code"])
        try makeApp(in: apps, "Safari.app", ["CFBundleExecutable": "Safari", "CFBundleName": "Safari"])
        try makeApp(in: apps.appendingPathComponent("Adobe Photoshop 2024"), "Adobe Photoshop 2024.app",
                    ["CFBundleExecutable": "Adobe Photoshop 2024"]) // no name keys: from the file name
        try makeApp(in: apps, "Broken.app", ["CFBundleName": "Broken"])          // no executable
        try makeApp(in: apps, "Finder.app", ["CFBundleExecutable": "Finder"])    // protected
        try makeApp(in: apps, "Hotfix.app", ["CFBundleExecutable": "Hotfix",
                                             "CFBundleIdentifier": AppCatalog.hotfixBundleID])
        try makeApp(in: user, "Safari Copy.app", ["CFBundleExecutable": "Safari"]) // same executable
        try "not an app".write(to: apps.appendingPathComponent("readme.txt"), atomically: true, encoding: .utf8)

        let found = AppCatalog.installedApps(in: [apps, user], excluding: ["Finder"])

        XCTAssertEqual(found, [
            InstalledApp(name: "Adobe Photoshop 2024", label: "Adobe Photoshop 2024"),
            InstalledApp(name: "Safari", label: "Safari"),
            InstalledApp(name: "Electron", label: "Visual Studio Code"),
        ])
    }

    func testInstalledAppsToleratesMissingDirectories() {
        XCTAssertEqual(AppCatalog.installedApps(in: [root.appendingPathComponent("nope")]), [])
    }

    // MARK: - Merging

    func testMergeOrdersRunningThenInstalled() {
        let gui = [
            RunningGUIApp(pid: 500, name: "Safari", label: "Safari"),
            RunningGUIApp(pid: 501, name: "Electron", label: nil), // label comes from the bundle
            RunningGUIApp(pid: 502, name: "Finder", label: "Finder"), // protected
        ]
        let ps: [(pid: Int32, cpu: Double, name: String)] = [
            (pid: 500, cpu: 12, name: "Safari"),
            (pid: 501, cpu: 40, name: "Electron"),
            (pid: 600, cpu: 30, name: "Safari Web Content"), // busy helper: listed
            (pid: 601, cpu: 0.2, name: "cfprefsd"),           // idle helper: hidden
            (pid: 50, cpu: 90, name: "launchd_child"),        // system PID: hidden
            (pid: 700, cpu: 5, name: "Electron"),             // another Electron process: summed
        ]
        let installed = [
            InstalledApp(name: "Blender", label: "Blender"),
            InstalledApp(name: "Electron", label: "Visual Studio Code"),
            InstalledApp(name: "Safari", label: "Safari"),
        ]

        let entries = AppCatalog.merge(gui: gui, ps: ps, installed: installed, excluded: ["Finder"])

        XCTAssertEqual(entries, [
            AppEntry(name: "Electron", label: "Visual Studio Code", cpu: 45, running: true),
            AppEntry(name: "Safari Web Content", label: nil, cpu: 30, running: true),
            AppEntry(name: "Safari", label: "Safari", cpu: 12, running: true),
            AppEntry(name: "Blender", label: "Blender", cpu: 0, running: false),
        ])
    }

    func testMergeKeepsIdleGUIApps() {
        // An open app is listed even when idle (it isn't a background helper).
        let entries = AppCatalog.merge(
            gui: [RunningGUIApp(pid: 800, name: "Notes", label: "Notes")],
            ps: [],
            installed: [],
            excluded: []
        )
        XCTAssertEqual(entries, [AppEntry(name: "Notes", label: "Notes", cpu: 0, running: true)])
    }

    // MARK: - Filtering

    func testFilterMatchesNameOrLabelCaseInsensitively() {
        let entries = [
            AppEntry(name: "Electron", label: "Visual Studio Code", cpu: 1, running: true),
            AppEntry(name: "Safari", label: "Safari", cpu: 0, running: false),
        ]
        XCTAssertEqual(AppCatalog.filter(entries, query: "studio").map(\.name), ["Electron"])
        XCTAssertEqual(AppCatalog.filter(entries, query: "ELECTRON").map(\.name), ["Electron"])
        XCTAssertEqual(AppCatalog.filter(entries, query: "  ").map(\.name), ["Electron", "Safari"])
        XCTAssertEqual(AppCatalog.filter(entries, query: "zzz"), [])
    }
}
