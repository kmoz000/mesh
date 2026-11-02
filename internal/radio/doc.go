// Package radio is WantasticMesh's radio capability model and low-level
// reconfiguration layer: the "full control over network/wifi config" goal.
//
// It uses the device's own vendor IPC and the kernel modules' netlink APIs plus
// standard nl80211 — never QCA vendor nl80211 commands and never dlopen/cgo
// against the bare vendor .so (which cannot be loaded; see
// research/VENDOR-IPC.md and research/BINARY-ANALYSIS.md). Concerns are kept in
// separate files so the pure logic compiles and tests on any host (including
// under TinyGo) while device-touching code is confined to //go:build linux
// files:
//
//   - Capability model (capability.go, sysfs.go, source.go): what radios a node
//     has and what each can do. Enumeration reads sysfs
//     (/sys/class/ieee80211/*/ and /sys/class/net/*/phy80211) with a
//     configurable root so tests need no real device; richer capability (per
//     channel lists, tx-power range, operating state, MLD membership) comes from
//     standard nl80211 and is filled only on Linux (probe_linux.go). MLDLink is
//     read with standard nl80211 GET_INTERFACE (MLD_ADDR / MLO_LINK_ID), which
//     is not a vendor command. Where telemetry needs a call that cannot run in a
//     test, the record is left at an honest zero and the Known bitmask marks the
//     field absent. Nothing is fabricated.
//
//   - Generic-netlink codec and client (genlmsg.go pure; genlsock_linux.go
//     socket): a pure-Go NETLINK_GENERIC client. It resolves a family by name
//     via CTRL_CMD_GETFAMILY — "nl80211" for standard telemetry/actuation and,
//     through family.go, the kernel-module families ("hyfi" from hyfi-bridging.ko,
//     "emesh" from emesh-sp.ko) whose ids differ per device and are never
//     hardcoded. Every buffer is bounded and every parse does explicit length
//     checks. No fmt on the message path.
//
//   - Vendor IPC trigger (vendoripc.go pure; vendoripc_linux.go FIFO): a thin
//     one-way trigger over the ezmesh control FIFOs
//     (/var/run/ezmesh-ctrl-cmd.fifo, /var/run/ezmesh-agent-cmd.fifo). It writes
//     a text verb (chanprefquery, topopt, peer_bss_info on the ctrl pipe;
//     bkscan on the agent pipe) and lets the vendor stack do the radio-safe
//     work. It is behind an interface so tests use a mock and open no real FIFO.
//
//   - Reconfiguration transaction (plan.go, rollback.go, transaction.go,
//     actuator_linux.go): the PREPARE -> READY -> APPLY -> VERIFY -> COMMIT
//     machine, with a rollback record persisted before any actuation and a
//     poll-driven watchdog (Tick) that reverts if COMMIT does not arrive before
//     a deadline. Retuning the only backhaul radio is refused unless a watchdog
//     deadline and a writable rollback store are present and a complete inverse
//     change can be built, so a change can never strand a node. All side effects
//     are behind interfaces (Actuator, Store, Clock) so the whole machine is
//     unit-tested against a mock.
//
// Ownership rules this package encodes:
//   - Channel/width changes go through hostapd_cli chan_switch (CSA, keeps
//     clients). The package never drives a raw nl80211 SET_CHANNEL behind
//     hostapd's back.
//   - Tx power goes through nl80211 SET_WIPHY (LIMITED, scoped to the wiphy),
//     never above the radio's reported range. It is verified by reading the
//     level back (NL80211_ATTR_WIPHY_TX_POWER_LEVEL, the same value `iw dev info`
//     prints) within +/-1 dBm, since the driver rounds to a regulatory step.
//     GuardedTxPower drives a reversible, watchdog-armed change over this path.
//   - Vendor ACS is assumed already disabled on the devices; this package never
//     drives it. FIFO triggers (which can make the vendor stack retune) are
//     refused while a transaction is open, so the two never run concurrently.
//
// DEVICE EXECUTION IS PENDING. The last SSH credentials failed and no live
// device was reached. Everything here is built and tested against mocks only:
// no live device, no SSH, no real hostapd_cli, no real FIFO, and no real
// netlink socket in tests. The nl80211 attribute/command ids and the genetlink
// control ids are confirmed from public headers (see ids.go). The ezmesh FIFO
// verb set, the kernel-module family ids, the exact MLO attribute shape, and the
// hostapd control-interface path must still be confirmed against the running
// device before any actuation path is trusted on hardware.
package radio
