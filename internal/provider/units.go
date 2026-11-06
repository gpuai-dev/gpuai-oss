package provider

// MiBToGB converts an upstream mebibyte figure to whole GB, rounding to
// nearest rather than truncating.
//
// Adapters were doing this with integer division (`ram / 1024`), which is
// exact only while every upstream reports whole multiples of 1024. The moment
// one doesn't, truncation under-reports the machine — the same class of defect
// as the RTX A4000 whose 16376 MiB of VRAM advertised as 15 GB. Rounding is
// correct in both directions and costs nothing.
//
// Non-positive input returns 0: a missing spec must stay missing (it renders
// as "-"), never become a negative or a fabricated value.
func MiBToGB(mib int) int {
	if mib <= 0 {
		return 0
	}
	return (mib + 512) / 1024
}
