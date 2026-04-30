import SwiftUI
import NarrowcastProtocol

struct ListenView: View {
    let server: Server

    @EnvironmentObject var store: ServerStore
    @StateObject private var vm = ConnectionViewModel()
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
            // Waterfall disabled — 1024-bin Canvas redraw at 10 fps pegged
            // the main thread. Re-enable behind a Metal texture / CGImage
            // row update.
            Spacer(minLength: 0)
            playStop
        }
        .padding()
        .navigationTitle(server.name)
        .navigationBarTitleDisplayMode(.inline)
        .onAppear {
            vm.connect(server: server, password: store.password(for: server.id))
        }
        .onDisappear {
            vm.disconnect()
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
        HStack {
            Circle().fill(stateColor).frame(width: 10, height: 10)
            Text(stateLabel).font(.caption).foregroundStyle(.secondary)
            Spacer()
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
        case .connecting: return "Connecting…"
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
        VStack(alignment: .leading, spacing: 4) {
            HStack {
                Text("S").font(.caption).foregroundStyle(.secondary)
                Spacer()
                Text("\(Int(vm.sMeterDb)) dB").font(.caption).monospacedDigit()
            }
            // Server pushes ~20 status samples/sec; SwiftUI interpolates
            // between them so the meter slides at display rate (60-120 fps)
            // instead of stepping in 50 ms blocks.
            ProgressView(value: sMeterFraction(vm.sMeterDb))
                .progressViewStyle(.linear)
                .tint(vm.sMeterDb > vm.squelchDb ? .green : .gray)
                .animation(.easeOut(duration: 0.08), value: vm.sMeterDb)
        }
    }

    private var squelch: some View {
        VStack(alignment: .leading, spacing: 4) {
            HStack {
                Text("Squelch").font(.caption).foregroundStyle(.secondary)
                Spacer()
                Text("\(Int(vm.squelchDb)) dB").font(.caption).monospacedDigit()
            }
            Slider(value: Binding(
                get: { Double(vm.squelchDb) },
                set: { vm.setSquelch(Float($0)) }
            ), in: -120...0)
        }
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

    private var waterfall: some View {
        WaterfallView(frames: vm.waterfallFrames) { frac in
            // Tap maps to a bin-fraction; convert to freq offset from current
            // center using the server-reported sample rate.
            guard let info = vm.serverInfo, vm.freqHz > 0 else { return }
            let sr = Double(info.sampleRate)
            let offset = (Double(frac) - 0.5) * sr
            let newHz = max(info.minHz, min(info.maxHz, UInt64(Int64(vm.freqHz) + Int64(offset))))
            vm.setFrequency(newHz)
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

    private func sMeterFraction(_ db: Float) -> Double {
        // Map -120..0 dB → 0..1
        let clamped = max(-120, min(0, db))
        return Double((clamped + 120) / 120)
    }

    private func formatFreq(_ hz: UInt64) -> String {
        if hz == 0 { return "—" }
        let mhz = Double(hz) / 1_000_000
        return String(format: "%.4f MHz", mhz)
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
        _text = State(initialValue: initial == 0 ? "" : String(format: "%.4f", Double(initial) / 1_000_000))
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
