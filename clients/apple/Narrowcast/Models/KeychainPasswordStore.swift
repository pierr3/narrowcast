import Foundation
import Security

// KeychainPasswordStore stores per-server passwords as `kSecClassGenericPassword`
// items keyed by server UUID. Replaces the cleartext UserDefaults stub.
//
// Account = server UUID string, Service = "com.pierr3.narrowcast.password".
// The pair forms the unique row key. Passwords are stored as UTF-8 bytes; the
// Keychain handles encryption-at-rest under the device passcode.
enum KeychainPasswordStore {

    private static let service = "com.pierr3.narrowcast.password"

    static func set(_ password: String, for id: UUID) {
        let account = id.uuidString
        let data = Data(password.utf8)

        let query: [String: Any] = [
            kSecClass as String: kSecClassGenericPassword,
            kSecAttrService as String: service,
            kSecAttrAccount as String: account,
            kSecAttrSynchronizable as String: kSecAttrSynchronizableAny,
        ]

        // SecItemUpdate fails if the item doesn't exist; SecItemAdd fails if
        // it does. Try update, fall back to add.
        let updateAttrs: [String: Any] = [kSecValueData as String: data]
        let updateStatus = SecItemUpdate(query as CFDictionary, updateAttrs as CFDictionary)
        if updateStatus == errSecItemNotFound {
            var addQuery = query
            addQuery[kSecValueData as String] = data
            // Synchronizable + accessible-when-unlocked (no ThisDeviceOnly)
            // makes the entry ride iCloud Keychain across the user's
            // signed-in devices. Server list + favourites already sync via
            // KVS; this completes the picture.
            addQuery[kSecAttrAccessible as String] = kSecAttrAccessibleWhenUnlocked
            addQuery[kSecAttrSynchronizable as String] = kCFBooleanTrue
            let addStatus = SecItemAdd(addQuery as CFDictionary, nil)
            if addStatus != errSecSuccess {
                NSLog("[narrowcast] keychain SET add failed for %@: status=%d", account, Int(addStatus))
            }
        } else if updateStatus != errSecSuccess {
            NSLog("[narrowcast] keychain SET update failed for %@: status=%d", account, Int(updateStatus))
        }
    }

    static func get(for id: UUID) -> String? {
        // kSecAttrSynchronizableAny matches both local-only and synced
        // entries — needed because some devices may have entries pushed
        // from iCloud Keychain while others wrote local-only first.
        let query: [String: Any] = [
            kSecClass as String: kSecClassGenericPassword,
            kSecAttrService as String: service,
            kSecAttrAccount as String: id.uuidString,
            kSecAttrSynchronizable as String: kSecAttrSynchronizableAny,
            kSecMatchLimit as String: kSecMatchLimitOne,
            kSecReturnData as String: true,
        ]
        var result: AnyObject?
        let status = SecItemCopyMatching(query as CFDictionary, &result)
        guard status == errSecSuccess, let data = result as? Data else {
            NSLog("[narrowcast] keychain GET miss for %@: status=%d", id.uuidString, Int(status))
            return nil
        }
        let pw = String(data: data, encoding: .utf8)
        NSLog("[narrowcast] keychain GET ok for %@: %d bytes", id.uuidString, data.count)
        return pw
    }

    static func remove(for id: UUID) {
        let query: [String: Any] = [
            kSecClass as String: kSecClassGenericPassword,
            kSecAttrService as String: service,
            kSecAttrAccount as String: id.uuidString,
            kSecAttrSynchronizable as String: kSecAttrSynchronizableAny,
        ]
        SecItemDelete(query as CFDictionary)
    }
}
