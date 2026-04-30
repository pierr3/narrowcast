import Foundation

// Thin wrapper over NSUbiquitousKeyValueStore. Same Data API as
// UserDefaults but values sync across the user's iCloud-signed-in devices.
// Limits: ~1 MB total per app, 1024 keys, 1 MB per value. Plenty for a
// server list + favourites.
//
// Behaviour:
//   - Local writes propagate to other devices within seconds (longer if
//     a device is asleep / offline).
//   - Reads return the local synced copy. After
//     NSUbiquitousKeyValueStoreDidChangeExternallyNotification, the cache
//     is updated and observers refresh.
//   - On first run on a new device, KVS may take a few seconds to pull
//     down existing values — expect briefly empty lists then fill.
enum iCloudKVStore {
    private static var store: NSUbiquitousKeyValueStore { .default }

    static func data(forKey key: String) -> Data? {
        store.data(forKey: key)
    }

    static func setData(_ data: Data?, forKey key: String) {
        if let data {
            store.set(data, forKey: key)
        } else {
            store.removeObject(forKey: key)
        }
        store.synchronize()
    }

    static func dictionary(forKey key: String) -> [String: String]? {
        store.dictionary(forKey: key) as? [String: String]
    }

    static func setDictionary(_ map: [String: String]?, forKey key: String) {
        if let map {
            store.set(map, forKey: key)
        } else {
            store.removeObject(forKey: key)
        }
        store.synchronize()
    }

    static func remove(_ key: String) {
        store.removeObject(forKey: key)
        store.synchronize()
    }

    /// Subscribe to remote-change notifications. The closure runs on the
    /// main queue — use it to refresh in-memory caches when another device
    /// pushes an update.
    static func observeRemoteChanges(_ handler: @escaping () -> Void) -> NSObjectProtocol {
        NotificationCenter.default.addObserver(
            forName: NSUbiquitousKeyValueStore.didChangeExternallyNotification,
            object: store,
            queue: .main
        ) { _ in handler() }
    }
}
