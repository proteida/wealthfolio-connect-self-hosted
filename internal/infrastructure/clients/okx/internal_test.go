package okx

import "testing"

func TestChainName_AllBranches(t *testing.T) {
	cases := map[string]string{
		"1":     "ETHEREUM",
		"56":    "BSC",
		"137":   "POLYGON",
		"42161": "ARBITRUM",
		"10":    "OPTIMISM",
		"8453":  "BASE",
		"43114": "AVALANCHE",
		"324":   "ZKSYNC",
		"59144": "LINEA",
		"5000":  "MANTLE",
		"9999":  "CHAIN-9999", // default
	}
	for in, want := range cases {
		if got := chainName(in); got != want {
			t.Errorf("chainName(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestShortAddr(t *testing.T) {
	if shortAddr("0x12") != "0x12" {
		t.Error("short address should pass through unchanged")
	}
	if got := shortAddr("0xabcdef0123456789"); got == "0xabcdef0123456789" {
		t.Error("long address should be truncated")
	}
}

func TestIsTONWallet(t *testing.T) {
	for _, address := range []string{
		"UQAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA",
		"EQAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA",
		"0:0123456789abcdef",
		"-1:0123456789abcdef",
	} {
		if !isTONWallet(address) {
			t.Errorf("isTONWallet(%q) = false, want true", address)
		}
	}
	for _, address := range []string{"0xabc", "bc1q8rm2w4hla65d0zy670zxdqane52xwca32yqz9r", ""} {
		if isTONWallet(address) {
			t.Errorf("isTONWallet(%q) = true, want false", address)
		}
	}
}

func TestFirstNonEmpty(t *testing.T) {
	if got := firstNonEmpty("", " ", "0.25", "1"); got != "0.25" {
		t.Fatalf("firstNonEmpty() = %q, want %q", got, "0.25")
	}
	if got := firstNonEmpty("", " "); got != "" {
		t.Fatalf("firstNonEmpty() = %q, want empty string", got)
	}
}
