import SwiftUI
import NarrowcastProtocol
import NarrowcastClient

struct ListenView: View {
    let server: Server

    @EnvironmentObject var store: ServerStore
    @EnvironmentObject var vm: ConnectionViewModel
    @State private var freqText: String = ""
    @State private var showFreqEditor = false

    var body: some View {
        VStack(spacing: 14) {
            statusBar
            FavoritesBar(vm: vm)
            freqDisplay
            modePicker
            sMeter
            squelch
            gainControl
            spectrumPanel
            Spacer(minLength: 0)
            playStop
        }
        .padding()
        .navigationTitle(server.name)
        .navigationBarTitleDisplayMode(.inline)
        .toolbar {
            ToolbarItem(placement: .topBarTrailing) {
                Button(role: .destructive) {
                    vm.disconnect()
                } label: {
                    Image(systemName: "xmark.circle")
                }
                .disabled({ if case .connected = vm.state { return false }; if case .connecting = vm.state { return false }; return true }())
            }
        }
        .onAppear {
            // Idempotent: connect() no-ops if already on this server.
            // Connection survives nav pops back to ServersView, so re-entry
            // is instant instead of a full QUIC re-handshake.
            vm.connect(server: server, password: store.password(for: server.id))
        }
        .sheet(isPresented: $showFreqEditor) {
            FrequencyEditor(initial: vm.freqHz) { hz in
                vm.setFrequency(hz)
            }
            .presentationDetents([.medium])
        }
    }

    @ViewBuilder
    private var statusBar: some View {
        HStack(spacing: 8) {
            Circle().fill(stateColor).frame(width: 10, height: 10)
            Text(stateLabel).font(.caption).foregroundStyle(.secondary)
            Spacer()
            if let latency = vm.audioLatencyMs {
                LatencyBadge(audioMs: latency, rttMs: vm.rttMs)
            }
            if let loss = vm.lastLoss {
                LossBadge(sample: loss)
            }
            if vm.clientCount > 0 {
                Image(systemName: "person.2.fill").font(.caption)
                Text("\(vm.clientCount)").font(.caption).monospacedDigit()
            }
        }
    }

    private var stateColor: Color {
        switch vm.state {
        case .connected: return .green
        case .connecting: return .yellow
        case .authFailed, .error: return .red
        case .disconnected, .idle: return .gray
        }
    }

    private var stateLabel: String {
        switch vm.state {
        case .idle: return "Idle"
        case .connecting(let stage): return stage
        case .connected: return "Connected"
        case .authFailed: return "Auth failed"
        case .error(let msg): return msg
        case .disconnected: return "Disconnected"
        }
    }

    private var freqDisplay: some View {
        Text(freqLabel)
            .font(.system(size: 42, weight: .semibold, design: .rounded))
            .monospacedDigit()
            .foregroundStyle(vm.freqHz == 0 ? .secondary : .primary)
            .frame(maxWidth: .infinity, minHeight: 64)
            .contentShape(Rectangle())
            .onTapGesture { showFreqEditor = true }
    }

    private var freqLabel: String {
        if vm.freqHz == 0 { return "Tap to tune" }
        return formatFreq(vm.freqHz)
    }

    private var modePicker: some View {
        Picker("Mode", selection: Binding(
            get: { vm.mode },
            set: { vm.setMode($0) }
        )) {
            ForEach(DemodMode.allCases, id: \.self) { m in
                Text(m.label).tag(m)
            }
        }
        .pickerStyle(.segmented)
    }

    private var sMeter: some View {
        SMeterView(meter: vm.meter, squelchDb: vm.squelchDb)
    }

    /// Squelch fine step. The slider is continuous, but 120 dB across a phone's
    /// width is ~0.34 dB per point, so precise values need buttons. 0.5 dB is
    /// about as fine as is meaningful — the noise floor itself wanders more than
    /// that between status frames.
    private static let squelchStep: Float = 0.5

    private var squelch: some View {
        VStack(alignment: .leading, spacing: 4) {
            HStack {
                Text("Squelch").font(.caption).foregroundStyle(.secondary)
                Spacer()
                squelchStatus
            }
            HStack(spacing: 10) {
                squelchNudge(-Self.squelchStep, icon: "minus")
                Slider(value: Binding(
                    get: { Double(vm.squelchDb) },
                    set: { vm.setSquelch(Float($0)) }
                ), in: -120...0)
                squelchNudge(Self.squelchStep, icon: "plus")
                autoSquelchButton
            }
        }
    }

    /// Reads out the threshold normally, and the calibration's progress or
    /// result while one is running. Showing the measured floor alongside the
    /// value it chose keeps the automatic behaviour inspectable — the whole
    /// point is that the meter and the gate share a scale, so seeing both makes
    /// the number meaningful rather than magic.
    @ViewBuilder
    private var squelchStatus: some View {
        if let cal = vm.calibration {
            if let done = cal.finished {
                Text(String(format: "floor %.0f → set %.1f dB", done.noiseFloorDb, done.thresholdDb))
                    .font(.caption).monospacedDigit().foregroundStyle(.tint)
            } else {
                Text("Measuring noise floor… \(cal.secondsRemaining)s")
                    .font(.caption).monospacedDigit().foregroundStyle(.secondary)
            }
        } else {
            // One decimal: the value was always fractional, the readout just
            // hid it by rounding to Int.
            Text(String(format: "%.1f dB", vm.squelchDb))
                .font(.caption).monospacedDigit()
        }
    }

    private var autoSquelchButton: some View {
        Button {
            if vm.calibration == nil {
                vm.autoSquelch()
            } else {
                vm.cancelCalibration()
            }
        } label: {
            Group {
                if vm.calibration?.finished == nil, vm.calibration != nil {
                    ProgressView().controlSize(.mini)
                } else {
                    Text("Auto").font(.caption.weight(.semibold))
                }
            }
            .frame(width: 44, height: 30)
            .background(Color(.tertiarySystemFill), in: .capsule)
        }
        .buttonStyle(.plain)
        .disabled(vm.state != .connected)
        .accessibilityLabel("Set squelch automatically")
    }

    private func squelchNudge(_ delta: Float, icon: String) -> some View {
        Button {
            vm.setSquelch(min(0, max(-120, vm.squelchDb + delta)))
        } label: {
            Image(systemName: icon)
                .font(.caption.weight(.semibold))
                .frame(width: 30, height: 30)
                .background(Color(.tertiarySystemFill), in: .circle)
        }
        .buttonStyle(.plain)
        // Hold to keep stepping, for walking the threshold onto a weak signal.
        .buttonRepeatBehavior(.enabled)
        .disabled(vm.state != .connected)
    }

    @ViewBuilder
    private var gainControl: some View {
        VStack(alignment: .leading, spacing: 4) {
            HStack {
                Text("Gain").font(.caption).foregroundStyle(.secondary)
                Spacer()
                Toggle("Auto", isOn: Binding(
                    get: { vm.autoGain },
                    set: { vm.setAutoGain($0) }
                ))
                .labelsHidden()
                .toggleStyle(.switch)
                Text(vm.autoGain ? "auto" : "\(Int(vm.manualGainDb)) dB")
                    .font(.caption).monospacedDigit().foregroundStyle(.secondary)
                    .frame(minWidth: 56, alignment: .trailing)
            }
            if !vm.autoGain {
                Slider(value: Binding(
                    get: { Double(vm.manualGainDb) },
                    set: { vm.setManualGain(Float($0)) }
                ), in: 0...49.6, step: 0.1)
            }
        }
    }

    private var spectrumPanel: some View {
        // Spectrum sits below all controls. Bars are centered on tunedFreq,
        // span = SDR sample rate. Edge labels use the server-reported sampleRate
        // so the values stay correct if the SDR rate changes.
        let span = Double(vm.serverInfo?.sampleRate ?? 960_000)
        let centerMhz = Double(vm.freqHz) / 1_000_000
        let halfSpanMhz = span / 2_000_000
        return VStack(spacing: 4) {
            ZStack {
                RoundedRectangle(cornerRadius: 10, style: .continuous)
                    .fill(Color(.secondarySystemBackground))
                SpectrumView(store: vm.spectrumStore)
                    .padding(.vertical, 4)
                    .padding(.horizontal, 2)
            }
            .frame(height: 130)
            .clipShape(RoundedRectangle(cornerRadius: 10, style: .continuous))

            HStack {
                Text(String(format: "%.3f", centerMhz - halfSpanMhz))
                Spacer()
                Text(String(format: "%.3f MHz", centerMhz))
                Spacer()
                Text(String(format: "%.3f", centerMhz + halfSpanMhz))
            }
            .font(.caption2.monospacedDigit())
            .foregroundStyle(.secondary)
            .padding(.horizontal, 4)
        }
    }

    private var playStop: some View {
        Button {
            if vm.streaming { vm.stopStreaming() } else { vm.startStreaming() }
        } label: {
            Image(systemName: vm.streaming ? "stop.circle.fill" : "play.circle.fill")
                .font(.system(size: 72))
        }
        .disabled(vm.state != .connected)
    }

    private func formatFreq(_ hz: UInt64) -> String {
        if hz == 0 { return "—" }
        let mhz = Double(hz) / 1_000_000
        return String(format: "%.3f MHz", mhz)
    }
}

// LatencyBadge shows how far behind reality the audio is, which is the number a
// listener actually feels, with the network round trip alongside it so a slow
// link can be told apart from a deep jitter buffer.
struct LatencyBadge: View {
    let audioMs: Int
    let rttMs: Int?

    // Two labelled values rather than one number and a bare second one. The
    // unlabelled version was a puzzle: nothing told you which was the delay you
    // hear and which was the network, and that distinction is the entire reason
    // both are shown.
    var body: some View {
        HStack(spacing: 6) {
            value("delay", audioMs, tint: color)
            if let rttMs {
                value("net", rttMs, tint: .secondary)
            }
        }
        .padding(.horizontal, 7)
        .padding(.vertical, 2)
        .background(Capsule().fill(Color(.tertiarySystemBackground)))
    }

    private func value(_ label: String, _ ms: Int, tint: Color) -> some View {
        HStack(spacing: 3) {
            Text(label)
                .font(.caption2)
                .foregroundStyle(.tertiary)
            Text("\(ms)")
                .font(.caption2.monospacedDigit())
                .foregroundStyle(tint)
            Text("ms")
                .font(.caption2)
                .foregroundStyle(.tertiary)
        }
        .accessibilityElement(children: .combine)
        .accessibilityLabel(label == "delay"
            ? "Audio delay \(ms) milliseconds"
            : "Network round trip \(ms) milliseconds")
    }

    /// Thresholds match what the audio path allows: the jitter buffer sheds
    /// above 400 ms, so anything past that is a struggling link, not buffering.
    private var color: Color {
        switch audioMs {
        case ..<250:  return .secondary
        case ..<500:  return .yellow
        default:      return .red
        }
    }
}

// LossBadge surfaces the QualityReport loop's measurement: a coloured dot
// scaled to severity + the audio loss percentage. The fft loss number
// matters too but is generally proportional to audio loss so showing both
// is noisy; tap-to-expand could surface fft + windowMs later.
struct LossBadge: View {
    let sample: LossTracker.Sample

    var body: some View {
        HStack(spacing: 4) {
            Circle()
                .fill(color)
                .frame(width: 6, height: 6)
            Text("\(sample.audioLossPct)%")
                .font(.caption2.monospacedDigit())
                .foregroundStyle(.secondary)
        }
        .padding(.horizontal, 6)
        .padding(.vertical, 2)
        .background(Capsule().fill(Color(.tertiarySystemBackground)))
    }

    private var color: Color {
        switch sample.audioLossPct {
        case 0..<5:   return .green
        case 5..<15:  return .yellow
        default:      return .red
        }
    }
}

struct FrequencyEditor: View {
    let initial: UInt64
    let commit: (UInt64) -> Void

    @Environment(\.dismiss) var dismiss
    @FocusState private var fieldFocused: Bool
    @State private var text: String

    init(initial: UInt64, commit: @escaping (UInt64) -> Void) {
        self.initial = initial
        self.commit = commit
        _text = State(initialValue: initial == 0 ? "" : String(format: "%.3f", Double(initial) / 1_000_000))
    }

    private var parsedHz: UInt64? {
        // Tolerate comma decimal separator (some locale keyboards send ',').
        let cleaned = text.replacingOccurrences(of: ",", with: ".")
        guard let mhz = Double(cleaned), mhz > 0 else { return nil }
        return UInt64(mhz * 1_000_000)
    }

    var body: some View {
        NavigationStack {
            VStack(spacing: 20) {
                Spacer().frame(height: 8)

                TextField("MHz", text: $text)
                    .font(.system(size: 44, weight: .semibold, design: .rounded))
                    .monospacedDigit()
                    .multilineTextAlignment(.center)
                    .keyboardType(.decimalPad)
                    .focused($fieldFocused)
                    .padding(.horizontal, 24)
                    .padding(.vertical, 16)
                    .background(Color(.tertiarySystemFill), in: .rect(cornerRadius: 14))
                    .padding(.horizontal)

                Text(hint)
                    .font(.caption)
                    .foregroundStyle(parsedHz == nil && !text.isEmpty ? .red : .secondary)

                Spacer()

                Button {
                    if let hz = parsedHz {
                        commit(hz)
                        dismiss()
                    }
                } label: {
                    Text("Tune")
                        .font(.headline)
                        .foregroundStyle(.white)
                        .frame(maxWidth: .infinity)
                        .padding(.vertical, 16)
                        .background(parsedHz == nil ? Color.gray : Color.accentColor, in: .rect(cornerRadius: 14))
                }
                .disabled(parsedHz == nil)
                .padding(.horizontal)
                .padding(.bottom, 12)
            }
            .navigationTitle("Frequency")
            .navigationBarTitleDisplayMode(.inline)
            .toolbar {
                ToolbarItem(placement: .cancellationAction) {
                    Button("Cancel") { dismiss() }
                }
            }
            .onAppear { fieldFocused = true }
        }
    }

    private var hint: String {
        if text.isEmpty { return "Enter a frequency in MHz (e.g. 144.800)" }
        if parsedHz == nil { return "Invalid number" }
        return "Press Tune to retune"
    }
}
