import SwiftUI

@main
struct NarrowcastApp: App {
    @StateObject private var serverStore = ServerStore()
    @StateObject private var favoritesStore = FavoritesStore()
    // Single ConnectionViewModel owned by the app, not the listen view, so
    // popping back to the servers list and tapping in again doesn't tear
    // down the QUIC connection + re-handshake from scratch every time.
    @StateObject private var connection = ConnectionViewModel()

    var body: some Scene {
        WindowGroup {
            RootView()
                .environmentObject(serverStore)
                .environmentObject(favoritesStore)
                .environmentObject(connection)
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
