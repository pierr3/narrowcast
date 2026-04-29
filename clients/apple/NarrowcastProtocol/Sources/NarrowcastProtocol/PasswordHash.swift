import Foundation
import CryptoKit

public enum PasswordHash {
    public static func sha256(_ password: String) -> Data {
        let bytes = Array(password.utf8)
        let digest = SHA256.hash(data: bytes)
        return Data(digest)
    }
}
