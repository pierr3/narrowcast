import SwiftUI

struct ServersView: View {
    @EnvironmentObject var store: ServerStore
    @State private var showAdd = false

    var body: some View {
        List {
            if store.servers.isEmpty {
                Text("No servers yet. Add one to start listening.")
                    .foregroundStyle(.secondary)
            }
            ForEach(store.servers) { server in
                NavigationLink(value: server) {
                    VStack(alignment: .leading, spacing: 2) {
                        Text(server.name).font(.headline)
                        Text(server.hostPort).font(.subheadline).foregroundStyle(.secondary)
                    }
                }
            }
            .onDelete { idx in
                idx.map { store.servers[$0] }.forEach { store.remove($0) }
            }
        }
        .navigationTitle("Servers")
        .toolbar {
            ToolbarItem(placement: .primaryAction) {
                Button { showAdd = true } label: { Image(systemName: "plus") }
            }
        }
        .sheet(isPresented: $showAdd) {
            NavigationStack { AddServerView() }
        }
        .navigationDestination(for: Server.self) { server in
            ListenView(server: server)
        }
    }
}

struct AddServerView: View {
    @EnvironmentObject var store: ServerStore
    @Environment(\.dismiss) var dismiss

    @State private var name = ""
    @State private var host = ""
    @State private var port: String = "443"
    @State private var requiresPassword = true
    @State private var allowSelfSigned = false
    @State private var password = ""

    var body: some View {
        Form {
            Section("Identity") {
                TextField("Friendly name", text: $name)
                    .textInputAutocapitalization(.words)
            }
            Section("Endpoint") {
                TextField("host or relay.example.com", text: $host)
                    .keyboardType(.URL)
                    .textInputAutocapitalization(.never)
                    .autocorrectionDisabled()
                TextField("port", text: $port)
                    .keyboardType(.numberPad)
                Toggle("Self-signed cert (direct Pi)", isOn: $allowSelfSigned)
            }
            Section("Auth") {
                Toggle("Password required", isOn: $requiresPassword)
                if requiresPassword {
                    SecureField("password", text: $password)
                }
            }
        }
        .navigationTitle("Add Server")
        .toolbar {
            ToolbarItem(placement: .cancellationAction) {
                Button("Cancel") { dismiss() }
            }
            ToolbarItem(placement: .primaryAction) {
                Button("Save") { save() }
                    .disabled(host.isEmpty || name.isEmpty)
            }
        }
    }

    private func save() {
        let p = UInt16(port) ?? 443
        let s = Server(name: name, host: host, port: p,
                       requiresPassword: requiresPassword,
                       allowSelfSigned: allowSelfSigned)
        store.add(s, password: requiresPassword ? password : nil)
        dismiss()
    }
}
