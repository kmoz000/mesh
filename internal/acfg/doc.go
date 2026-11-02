// Package acfg will bind the device's QCA libacfg.so (the cfg80211 vendor
// configuration library) to create and delete Wi-Fi VAPs (vdevs) from inside
// wmeshd, with no `wifi` reload. On this QSDK build this is the only supported
// vdev-create path: standard nl80211 NEW_INTERFACE (iw) returns EINVAL and
// wlanconfig's legacy ioctl is a silent no-op; the vendor stack itself creates
// VAPs through libacfg (acfg_wlan_vap_create → cfg80211 vendor "vap create").
//
// Status: reverse-engineering in progress (research/ACFG-BINDING.md). The export
// surface, the acfg_wlan_vap_create / acfg_init_vap_params signatures, and the
// vap-params struct size/init pattern are recovered; the exact vap-params and
// radio-handle struct ABIs and the acfg_set_vap_param id enum are being
// reconstructed (rizin on libacfg.so + the acfg_set_profile binary, or the
// qca-wifi acfg_api.h). No acfg create call is made against a live device until
// the struct ABI is verified, to avoid crashing the vendor Wi-Fi stack.
//
// Build: link the stub at vendor-sysroot/stublib/libacfg.so with -lacfg; the
// device's real libacfg satisfies the symbols at runtime (NEEDED: libacfg.so).
package acfg
