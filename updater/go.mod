// The updater is deliberately a standalone module with no dependencies — not even
// on the main CLIProxyAPI module. Online update is the one component that must keep
// working across arbitrary refactors of the application it updates, so the coupling
// surface is reduced to the versioned wire contract in ./protocol.
//
// Do not add requires here. If the updater needs something from the application,
// it belongs in the protocol as data, not as a Go import.
module clirelay.local/updater

go 1.26.0
