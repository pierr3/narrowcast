import Foundation

/// SquelchCalibrator watches the S-meter for a few seconds and works out where
/// the squelch threshold belongs.
///
/// This can be done entirely on the client because of an invariant the server
/// deliberately maintains: the squelch gates on channel power measured before
/// demodulation, and the S-meter in every status frame reports *that same
/// quantity*. The number on the meter is the number the gate compares against,
/// so a threshold derived from meter samples means exactly what it says.
///
/// The estimate is a low percentile rather than a mean or a median, because a
/// transmission during the sampling window would drag either of those upward
/// and leave the threshold set above the signals it is supposed to pass.
///
/// Which percentile follows from that directly. With nearest-rank selection the
/// chosen sample is still noise only while the channel is busy less than
/// (1 - percentile) of the time, so the 10th percentile calibrates correctly on
/// a channel busy up to ~90%. Going lower buys nothing: on a quiet channel every
/// sample is noise and the low percentiles differ by a fraction of a dB, which
/// the margin below dwarfs.
public struct SquelchCalibrator: Sendable {

    public struct Result: Sendable, Equatable {
        /// Estimated noise floor, in the same dB units as the S-meter.
        public let noiseFloorDb: Float
        /// Where the gate should sit.
        public let thresholdDb: Float
        public let sampleCount: Int
    }

    /// How far above the noise floor the gate sits.
    ///
    /// The server closes the gate 3 dB below the threshold (hysteresis), so this
    /// has to clear that with room to spare, while staying low enough to pass a
    /// weak but readable signal.
    public static let defaultMarginDb: Float = 6

    /// Fraction of samples taken to be noise. See the type comment.
    private static let floorPercentile = 0.10

    /// Below this, a percentile is not meaningfully different from picking a
    /// sample at random. Three seconds of 10 Hz status frames is 30.
    public static let minimumSamples = 12

    private let marginDb: Float
    private var samples: [Float] = []

    public init(marginDb: Float = SquelchCalibrator.defaultMarginDb) {
        self.marginDb = marginDb
    }

    public mutating func add(powerDb: Float) {
        guard powerDb.isFinite else { return }
        samples.append(powerDb)
    }

    public var sampleCount: Int { samples.count }

    public var hasEnoughSamples: Bool { samples.count >= Self.minimumSamples }

    /// The threshold implied by the samples so far, or nil if there are too few
    /// to be worth acting on.
    public var result: Result? {
        guard hasEnoughSamples else { return nil }
        let sorted = samples.sorted()
        // Nearest-rank, clamped: index 0 for tiny sets rather than running off
        // the end of the array.
        let rank = min(sorted.count - 1, Int(Double(sorted.count) * Self.floorPercentile))
        let floor = sorted[rank]
        return Result(noiseFloorDb: floor,
                      thresholdDb: floor + marginDb,
                      sampleCount: sorted.count)
    }

    public mutating func reset() {
        samples.removeAll(keepingCapacity: true)
    }
}
