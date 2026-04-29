import SwiftUI
import NarrowcastProtocol

struct ListenView: View {
    let server: Server

    @EnvironmentObject var store: ServerStore
    @StateObject private var vm = ConnectionViewModel()
    @State private var freqText: String = ""
    @State private var showFreqEditor = false

    var body: some View {
        VStack(spacing: 16) {
            statusBar
            Spacer().frame(height: 8)
            freqDisplay
            modePicker
            sMeter
            squelch
            Spacer()
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
        Button {
            showFreqEditor = true
        } label: {
            Text(formatFreq(vm.freqHz))
                .font(.system(size: 44, weight: .semibold, design: .rounded))
                .monospacedDigit()
                .foregroundStyle(.primary)
        }
        .buttonStyle(.plain)
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
            ProgressView(value: sMeterFraction(vm.sMeterDb))
                .progressViewStyle(.linear)
                .tint(vm.sMeterDb > vm.squelchDb ? .green : .gray)
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
    @State private var text: String

    init(initial: UInt64, commit: @escaping (UInt64) -> Void) {
        self.initial = initial
        self.commit = commit
        _text = State(initialValue: initial == 0 ? "" : String(format: "%.4f", Double(initial) / 1_000_000))
    }

    var body: some View {
        NavigationStack {
            VStack {
                TextField("MHz", text: $text)
                    .font(.system(size: 36, weight: .semibold, design: .rounded))
                    .multilineTextAlignment(.center)
                    .keyboardType(.decimalPad)
                    .padding()
                Spacer()
            }
            .navigationTitle("Frequency")
            .navigationBarTitleDisplayMode(.inline)
            .toolbar {
                ToolbarItem(placement: .cancellationAction) {
                    Button("Cancel") { dismiss() }
                }
                ToolbarItem(placement: .primaryAction) {
                    Button("Tune") {
                        if let mhz = Double(text) {
                            commit(UInt64(mhz * 1_000_000))
                        }
                        dismiss()
                    }
                }
            }
        }
    }
}
