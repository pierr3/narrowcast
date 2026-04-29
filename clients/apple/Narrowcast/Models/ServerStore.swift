import Foundation
import SwiftUI

// In-memory + UserDefaults persistence for saved servers. Password lookup is
// stubbed against an in-memory dictionary for now; Keychain integration in
// Phase 10.
@MainActor
final class ServerStore: ObservableObject {
    @Published var servers: [Server] = []
    private var passwords: [UUID: String] = [:]

    private let key = "narrowcast.servers.v1"

    init() {
        load()
    }

    func add(_ s: Server, password: String?) {
        servers.append(s)
        if let password { passwords[s.id] = password }
        save()
    }

    func update(_ s: Server, password: String?) {
        if let i = servers.firstIndex(where: { $0.id == s.id }) {
            servers[i] = s
        }
        if let password { passwords[s.id] = password }
        save()
    }

    func remove(_ s: Server) {
        servers.removeAll { $0.id == s.id }
        passwords.removeValue(forKey: s.id)
        save()
    }

    func password(for id: UUID) -> String? { passwords[id] }

    private func save() {
        if let data = try? JSONEncoder().encode(servers) {
            UserDefaults.standard.set(data, forKey: key)
        }
    }

    private func load() {
        guard let data = UserDefaults.standard.data(forKey: key),
              let decoded = try? JSONDecoder().decode([Server].self, from: data) else { return }
        servers = decoded
    }
}
