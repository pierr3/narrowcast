import Foundation
import SwiftUI

// Server list synced via NSUbiquitousKeyValueStore. Per-server passwords
// live in Keychain with kSecAttrSynchronizable so they ride iCloud
// Keychain across devices.
@MainActor
final class ServerStore: ObservableObject {
    @Published var servers: [Server] = []

    private let serversKey = "narrowcast.servers.v1"
    private let legacyPasswordsKey = "narrowcast.passwords.v1"
    private var observer: NSObjectProtocol?

    init() {
        load()
        migrateLegacyPasswordsIfPresent()
        migrateLegacyServersIfPresent()
        observer = iCloudKVStore.observeRemoteChanges { [weak self] in
            self?.load()
        }
    }

    deinit {
        if let observer { NotificationCenter.default.removeObserver(observer) }
    }

    func add(_ s: Server, password: String?) {
        servers.append(s)
        if let password, !password.isEmpty {
            NSLog("[narrowcast] ServerStore.add storing password (%d chars) for server %@", password.count, s.id.uuidString)
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
            iCloudKVStore.setData(data, forKey: serversKey)
        }
    }

    private func load() {
        guard let data = iCloudKVStore.data(forKey: serversKey),
              let decoded = try? JSONDecoder().decode([Server].self, from: data) else { return }
        servers = decoded
    }

    /// Pull anything left over from the pre-iCloud UserDefaults store into
    /// KVS the first time we run with sync enabled. Wipes the legacy key.
    private func migrateLegacyServersIfPresent() {
        guard let legacy = UserDefaults.standard.data(forKey: serversKey),
              let decoded = try? JSONDecoder().decode([Server].self, from: legacy) else { return }
        if servers.isEmpty {
            servers = decoded
            save()
        }
        UserDefaults.standard.removeObject(forKey: serversKey)
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
