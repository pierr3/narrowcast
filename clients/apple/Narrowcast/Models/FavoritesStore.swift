import Foundation
import SwiftUI

@MainActor
final class FavoritesStore: ObservableObject {
    @Published var favorites: [Favorite] = []

    private let key = "narrowcast.favorites.v1"

    init() {
        load()
    }

    func add(_ f: Favorite) {
        favorites.append(f)
        save()
    }

    func remove(id: UUID) {
        favorites.removeAll { $0.id == id }
        save()
    }

    func update(_ f: Favorite) {
        if let i = favorites.firstIndex(where: { $0.id == f.id }) {
            favorites[i] = f
            save()
        }
    }

    private func save() {
        if let data = try? JSONEncoder().encode(favorites) {
            UserDefaults.standard.set(data, forKey: key)
        }
    }

    private func load() {
        guard let data = UserDefaults.standard.data(forKey: key),
              let decoded = try? JSONDecoder().decode([Favorite].self, from: data) else { return }
        favorites = decoded
    }
}
