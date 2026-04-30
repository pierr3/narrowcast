import SwiftUI
import NarrowcastProtocol

// Horizontal scrolling bar of saved presets above the freq display. One tap
// applies freq + mode + squelch + gain. Long press opens a context menu to
// remove. Plus button captures the current state into a new entry.
struct FavoritesBar: View {
    @EnvironmentObject var store: FavoritesStore
    @ObservedObject var vm: ConnectionViewModel

    @State private var showAdd = false

    var body: some View {
        ScrollView(.horizontal, showsIndicators: false) {
            HStack(spacing: 10) {
                ForEach(store.favorites) { fav in
                    chip(for: fav)
                }
                if store.favorites.isEmpty {
                    emptyHint
                }
                addChip
            }
            .padding(.horizontal, 4)
            .padding(.vertical, 4)
        }
        .frame(maxWidth: .infinity)
        .sheet(isPresented: $showAdd) {
            AddFavoriteSheet(vm: vm)
                .presentationDetents([.medium])
        }
    }

    private func chip(for fav: Favorite) -> some View {
        Button {
            vm.applyFavorite(fav)
        } label: {
            VStack(alignment: .leading, spacing: 2) {
                Text(fav.name)
                    .font(.subheadline.weight(.semibold))
                    .lineLimit(1)
                    .foregroundStyle(.primary)
                HStack(spacing: 6) {
                    Text(String(format: "%.3f MHz", fav.freqMHz))
                        .monospacedDigit()
                    Text("•").foregroundStyle(.tertiary)
                    Text(fav.mode.label)
                }
                .font(.caption)
                .foregroundStyle(.secondary)
            }
            .padding(.horizontal, 14)
            .padding(.vertical, 10)
            .frame(minWidth: 130, alignment: .leading)
            .background(Color(.tertiarySystemFill), in: .rect(cornerRadius: 12))
        }
        .buttonStyle(.plain)
        .contextMenu {
            Button(role: .destructive) {
                store.remove(id: fav.id)
            } label: {
                Label("Remove", systemImage: "trash")
            }
        }
    }

    private var emptyHint: some View {
        Text("Tap + to save the current freq")
            .font(.caption)
            .foregroundStyle(.secondary)
            .padding(.horizontal, 8)
            .padding(.vertical, 10)
    }

    private var addChip: some View {
        Button {
            showAdd = true
        } label: {
            Image(systemName: "plus")
                .font(.title3.weight(.semibold))
                .foregroundStyle(.tint)
                .frame(width: 44, height: 44)
                .background(Color(.tertiarySystemFill), in: .circle)
        }
        .buttonStyle(.plain)
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
                    LabeledContent("Frequency", value: String(format: "%.3f MHz", Double(vm.freqHz) / 1_000_000))
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
        String(format: "%.3f %@", Double(vm.freqHz) / 1_000_000, vm.mode.label)
    }
}
