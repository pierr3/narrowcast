import SwiftUI

@main
struct NarrowcastApp: App {
    @StateObject private var serverStore = ServerStore()

    var body: some Scene {
        WindowGroup {
            RootView()
                .environmentObject(serverStore)
        }
    }
}

struct RootView: View {
    @EnvironmentObject var store: ServerStore

    var body: some View {
        NavigationStack {
            ServersView()
        }
    }
}
