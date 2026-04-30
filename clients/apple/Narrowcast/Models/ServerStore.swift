import Foundation
import SwiftUI

// Server list in UserDefaults; per-server passwords in Keychain.
// On first launch after the Keychain switch, any cleartext password left
// behind by the older UserDefaults stub is migrated and the old key wiped.
@MainActor
final class ServerStore: ObservableObject {
    @Published var servers: [Server] = []

    private let serversKey = "narrowcast.servers.v1"
    private let legacyPasswordsKey = "narrowcast.passwords.v1"

    init() {
        load()
        migrateLegacyPasswordsIfPresent()
    }

    func add(_ s: Server, password: String?) {
        servers.append(s)
        if let password, !password.isEmpty {
            KeychainPasswordStore.set(password, for: s.id)
        }
        save()
    }

    func update(_ s: Server, password: String?) {
        if let i = servers.firstIndex(where: { $0.id == s.id }) {
            servers[i] = s
        }
        if let password, !password.isEmpty {
            KeychainPasswordStore.set(password, for: s.id)
        }
        save()
    }

    func remove(_ s: Server) {
        servers.removeAll { $0.id == s.id }
        KeychainPasswordStore.remove(for: s.id)
        save()
    }

    func password(for id: UUID) -> String? {
        KeychainPasswordStore.get(for: id)
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

    private func migrateLegacyPasswordsIfPresent() {
        guard let legacy = UserDefaults.standard.dictionary(forKey: legacyPasswordsKey) as? [String: String] else {
            return
        }
        for (idStr, pw) in legacy where !pw.isEmpty {
            if let uuid = UUID(uuidString: idStr) {
                KeychainPasswordStore.set(pw, for: uuid)
            }
        }
        UserDefaults.standard.removeObject(forKey: legacyPasswordsKey)
    }
}
