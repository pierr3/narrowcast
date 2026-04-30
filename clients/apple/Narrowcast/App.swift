import SwiftUI

@main
struct NarrowcastApp: App {
    @StateObject private var serverStore = ServerStore()
    @StateObject private var favoritesStore = FavoritesStore()
    // Single ConnectionViewModel owned by the app, not the listen view, so
    // popping back to the servers list and tapping in again doesn't tear
    // down the QUIC connection + re-handshake from scratch every time.
    @StateObject private var connection = ConnectionViewModel()
    @Environment(\.scenePhase) private var scenePhase

    var body: some Scene {
        WindowGroup {
            RootView()
                .environmentObject(serverStore)
                .environmentObject(favoritesStore)
                .environmentObject(connection)
        }
        .onChange(of: scenePhase) { _, phase in
            // .background fires when the user swipes away or locks the
            // screen. Without an explicit disconnect, iOS suspends the
            // process and the QUIC connection lingers as a zombie on the
            // relay until its idle timeout (relay would otherwise show
            // "(2 clients)" for one physical phone after a re-launch).
            // Note: doesn't fire on force-quit (the OS kills us mid-air).
            if phase == .background {
                connection.disconnect()
            }
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
