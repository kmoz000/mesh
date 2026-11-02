package radio

// Protocol ids used by the netlink codec and the actuation layer.
//
// Provenance. Every value here was read from public headers during development,
// not recalled:
//
//   - Standard nl80211: enum nl80211_commands / nl80211_attrs / nl80211_chan_width
//     / nl80211_tx_power_setting / nl80211_band_attr / nl80211_frequency_attr in
//     torvalds/linux include/uapi/linux/nl80211.h (master). Alias members such as
//     NL80211_CMD_NEW_BEACON = NL80211_CMD_START_AP reuse a value and were not
//     counted when computing positions.
//   - Generic netlink control: include/uapi/linux/genetlink.h (master).
//
// This package deliberately carries NO QCA vendor nl80211 ids. An earlier
// attempt routed channel/power/telemetry through QCA vendor commands (OUI
// 0x001374, SUBCMD_SET/GET_WIFI_CONFIGURATION, and unverified QSDK setparam /
// getparam ids reverse-engineered from a vendor .so). That path was rejected:
// the vendor framing is not provable from the inventory and the ids differ per
// QSDK build (see research/VENDOR-IPC.md and research/NL80211-VENDOR-MAP.md).
// Channel/width now go through hostapd_cli CSA, tx power through the standard
// NL80211_CMD_SET_WIPHY below, vendor channel-preference / backhaul-scan work is
// *triggered* through the ezmesh control FIFOs (see vendoripc.go), and the
// hyfi / emesh-sp kernel-module families are resolved by name at runtime (see
// family.go). No NL80211_CMD_VENDOR envelope is built anywhere in this package.

// Generic netlink control family (nlctrl).
const (
	genlIDCtrl           = 0x10 // GENL_ID_CTRL == NLMSG_MIN_TYPE. CONFIRMED.
	ctrlCmdGetFamily     = 3    // CTRL_CMD_GETFAMILY. CONFIRMED.
	ctrlAttrFamilyID     = 1    // CTRL_ATTR_FAMILY_ID (u16). CONFIRMED.
	ctrlAttrFamilyName   = 2    // CTRL_ATTR_FAMILY_NAME (string). CONFIRMED.
	ctrlAttrMcastGroups  = 7    // CTRL_ATTR_MCAST_GROUPS (nested). CONFIRMED.
	ctrlAttrMcastGrpName = 1    // CTRL_ATTR_MCAST_GRP_NAME. CONFIRMED.
	ctrlAttrMcastGrpID   = 2    // CTRL_ATTR_MCAST_GRP_ID. CONFIRMED.

	genlFamilyNameNL80211 = "nl80211"

	// Kernel-module generic-netlink family names. Their family ids are resolved
	// per device with CTRL_CMD_GETFAMILY and are never hardcoded (hashes differ
	// across devices). See family.go. The EXACT registered family-name strings
	// (the .ko's genl_family.name field) are PENDING device confirmation — these
	// are placeholders, not readings; confirm on device with
	// `strings <module.ko> | grep -i <name>` or `genl ctrl list`. No compiled
	// caller uses these yet.
	genlFamilyNameHyFi    = "hyfi"  // hyfi-bridging.ko (placeholder; PENDING)
	genlFamilyNameEmeshSP = "emesh" // emesh-sp.ko (placeholder; PENDING)
)

// nl80211 commands. CONFIRMED.
const (
	nl80211CmdGetWiphy      = 1
	nl80211CmdSetWiphy      = 2
	nl80211CmdGetInterface  = 5
	nl80211CmdGetStation    = 17
	nl80211CmdGetSurvey     = 50
	nl80211CmdChannelSwitch = 102
)

// nl80211 attributes. CONFIRMED.
const (
	nl80211AttrWiphy             = 1
	nl80211AttrIfIndex           = 3
	nl80211AttrIfName            = 4
	nl80211AttrIfType            = 5
	nl80211AttrMAC               = 6
	nl80211AttrWiphyBands        = 22
	nl80211AttrWiphyFreq         = 38
	nl80211AttrWiphyChannelType  = 39
	nl80211AttrWiphyTxPowerSetng = 97
	nl80211AttrWiphyTxPowerLevel = 98
	nl80211AttrChannelWidth      = 159
	nl80211AttrCenterFreq1       = 160
	nl80211AttrSplitWiphyDump    = 174
	nl80211AttrMLOLinks          = 312
	nl80211AttrMLOLinkID         = 313
	nl80211AttrMLDAddr           = 314
)

// nl80211 nested band / frequency attributes (inside NL80211_ATTR_WIPHY_BANDS).
// CONFIRMED.
const (
	nl80211BandAttrFreqs = 1 // NL80211_BAND_ATTR_FREQS

	nl80211FreqAttrFreq       = 1 // NL80211_FREQUENCY_ATTR_FREQ (u32 MHz)
	nl80211FreqAttrDisabled   = 2 // flag
	nl80211FreqAttrNoIR       = 3 // flag
	nl80211FreqAttrRadar      = 5 // flag
	nl80211FreqAttrMaxTxPower = 6 // u32 mBm
)

// nl80211_chan_width -> value. CONFIRMED.
const (
	chanWidth20NoHT = 0
	chanWidth20     = 1
	chanWidth40     = 2
	chanWidth80     = 3
	chanWidth80P80  = 4
	chanWidth160    = 5
	chanWidth320    = 13
)

// nl80211_tx_power_setting. CONFIRMED.
const (
	txPowerAutomatic = 0
	txPowerLimited   = 1
	txPowerFixed     = 2
)
