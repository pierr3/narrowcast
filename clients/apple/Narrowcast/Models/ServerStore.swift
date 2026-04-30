import Foundation
import SwiftUI

// Server list + passwords persisted in UserDefaults. Passwords in
// UserDefaults is insecure — Keychain replaces this in Phase 10. Until then
// this is the cheapest way to keep dev rebuilds usable across launches.
@MainActor
final class ServerStore: ObservableObject {
    @Published var servers: [Server] = []

    private let serversKey = "narrowcast.servers.v1"
    private let passwordsKey = "narrowcast.passwords.v1"

    init() {
        load()
    }

    func add(_ s: Server, password: String?) {
        servers.append(s)
        if let password, !password.isEmpty {
            setPassword(password, for: s.id)
        }
        save()
    }

    func update(_ s: Server, password: String?) {
        if let i = servers.firstIndex(where: { $0.id == s.id }) {
            servers[i] = s
        }
        if let password, !password.isEmpty {
            setPassword(password, for: s.id)
        }
        save()
    }

    func remove(_ s: Server) {
        servers.removeAll { $0.id == s.id }
        var map = loadPasswords()
        map.removeValue(forKey: s.id.uuidString)
        savePasswords(map)
        save()
    }

    func password(for id: UUID) -> String? {
        let map = loadPasswords()
        let pw = map[id.uuidString]
        return (pw?.isEmpty ?? true) ? nil : pw
    }

    private func setPassword(_ pw: String, for id: UUID) {
        var map = loadPasswords()
        map[id.uuidString] = pw
        savePasswords(map)
    }

    private func loadPasswords() -> [String: String] {
        (UserDefaults.standard.dictionary(forKey: passwordsKey) as? [String: String]) ?? [:]
    }

    private func savePasswords(_ map: [String: String]) {
        UserDefaults.standard.set(map, forKey: passwordsKey)
    }

    private func save() {
        if let data = try? JSONEncoder().encode(servers) {
            UserDefaults.standard.set(data, forKey: serversKey)
        }
    }

    private func load() {
        guard let data = UserDefaults.standard.data(forKey: serversKey),
              let decoded = try? JSONDecoder().decode([Server].self, from: data) else { return }
        servers = decoded
    }
}
