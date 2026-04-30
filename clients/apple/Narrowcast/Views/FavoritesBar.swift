import SwiftUI
import NarrowcastProtocol

// Horizontal scrolling bar of saved presets above the freq display. One tap
// applies freq + mode + squelch + gain to the connected server. Long press
// removes. Plus button captures the current state into a new entry.
struct FavoritesBar: View {
    @EnvironmentObject var store: FavoritesStore
    @ObservedObject var vm: ConnectionViewModel

    @State private var showAdd = false

    var body: some View {
        ScrollView(.horizontal, showsIndicators: false) {
            HStack(spacing: 8) {
                ForEach(store.favorites) { fav in
                    chip(for: fav)
                }
                addChip
            }
            .padding(.horizontal, 4)
            .padding(.vertical, 4)
        }
        .frame(maxWidth: .infinity)
        .sheet(isPresented: $showAdd) {
            AddFavoriteSheet(vm: vm)
                .presentationDetents([.height(220), .medium])
        }
    }

    private func chip(for fav: Favorite) -> some View {
        Button {
            vm.applyFavorite(fav)
        } label: {
            VStack(alignment: .leading, spacing: 1) {
                Text(fav.name)
                    .font(.caption.weight(.semibold))
                    .lineLimit(1)
                Text(String(format: "%.4f %@", fav.freqMHz, fav.mode.label))
                    .font(.caption2)
                    .monospacedDigit()
                    .foregroundStyle(.secondary)
            }
            .padding(.horizontal, 12)
            .padding(.vertical, 8)
        }
        .buttonStyle(.plain)
        .glassEffect(.regular, in: .capsule)
        .contextMenu {
            Button(role: .destructive) {
                store.remove(id: fav.id)
            } label: {
                Label("Remove", systemImage: "trash")
            }
        }
    }

    private var addChip: some View {
        Button {
            showAdd = true
        } label: {
            Image(systemName: "plus")
                .font(.caption.weight(.bold))
                .padding(12)
        }
        .buttonStyle(.plain)
        .glassEffect(.regular.interactive(), in: .circle)
        .disabled(vm.freqHz == 0)
    }
}

struct AddFavoriteSheet: View {
    @ObservedObject var vm: ConnectionViewModel
    @EnvironmentObject var store: FavoritesStore
    @Environment(\.dismiss) var dismiss
    @State private var name: String = ""

    var body: some View {
        NavigationStack {
            Form {
                Section("Name") {
                    TextField(autoName, text: $name)
                }
                Section("Snapshot") {
                    LabeledContent("Frequency", value: String(format: "%.4f MHz", Double(vm.freqHz) / 1_000_000))
                    LabeledContent("Mode", value: vm.mode.label)
                    LabeledContent("Squelch", value: "\(Int(vm.squelchDb)) dB")
                    LabeledContent("Gain", value: vm.autoGain ? "auto" : "\(Int(vm.manualGainDb)) dB")
                }
            }
            .navigationTitle("New Favorite")
            .navigationBarTitleDisplayMode(.inline)
            .toolbar {
                ToolbarItem(placement: .cancellationAction) {
                    Button("Cancel") { dismiss() }
                }
                ToolbarItem(placement: .primaryAction) {
                    Button("Save") {
                        let n = name.trimmingCharacters(in: .whitespaces)
                        let fav = vm.currentSnapshotForFavorite(name: n.isEmpty ? autoName : n)
                        store.add(fav)
                        dismiss()
                    }
                }
            }
        }
    }

    private var autoName: String {
        String(format: "%.4f %@", Double(vm.freqHz) / 1_000_000, vm.mode.label)
    }
}
