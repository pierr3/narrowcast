import SwiftUI

@main
struct NarrowcastApp: App {
    @StateObject private var serverStore = ServerStore()
    @StateObject private var favoritesStore = FavoritesStore()

    var body: some Scene {
        WindowGroup {
            RootView()
                .environmentObject(serverStore)
                .environmentObject(favoritesStore)
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
