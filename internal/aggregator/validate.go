package aggregator

import (
	"fmt"
	"net/netip"
)

// Минимальная длина маски IPv4: /9 и уже — ок, /8 и шире — отклоняем.
const minV4Bits = 9
const minV6Bits = 32

var reservedV4 = func() []netip.Prefix {
	raw := []string{
		"0.0.0.0/8", "10.0.0.0/8", "100.64.0.0/10", "127.0.0.0/8",
		"169.254.0.0/16", "172.16.0.0/12", "192.0.0.0/24", "192.0.2.0/24",
		"192.88.99.0/24", "192.168.0.0/16", "198.18.0.0/15", "198.51.100.0/24",
		"203.0.113.0/24", "224.0.0.0/4", "240.0.0.0/4", "255.255.255.255/32",
	}
	out := make([]netip.Prefix, 0, len(raw))
	for _, s := range raw {
		out = append(out, netip.MustParsePrefix(s))
	}
	return out
}()

var reservedV6 = func() []netip.Prefix {
	raw := []string{
		"::/128", "::1/128", "::ffff:0:0/96", "64:ff9b::/96",
		"100::/64", "2001:db8::/32", "fc00::/7", "fe80::/10", "ff00::/8",
	}
	out := make([]netip.Prefix, 0, len(raw))
	for _, s := range raw {
		out = append(out, netip.MustParsePrefix(s))
	}
	return out
}()

func Validate(p netip.Prefix) error {
	if !p.IsValid() {
		return fmt.Errorf("invalid prefix")
	}
	p = p.Masked()
	if p.Addr().Is4() {
		if p.Bits() < minV4Bits {
			return fmt.Errorf("prefix %s too wide (min /%d)", p, minV4Bits)
		}
		for _, r := range reservedV4 {
			if r.Overlaps(p) {
				return fmt.Errorf("prefix %s overlaps reserved %s", p, r)
			}
		}
		return nil
	}
	if p.Bits() < minV6Bits {
		return fmt.Errorf("prefix %s too wide (min /%d)", p, minV6Bits)
	}
	for _, r := range reservedV6 {
		if r.Overlaps(p) {
			return fmt.Errorf("prefix %s overlaps reserved %s", p, r)
		}
	}
	return nil
}

func FilterValid(in []netip.Prefix) ([]netip.Prefix, []error) {
	out := make([]netip.Prefix, 0, len(in))
	var errs []error
	for _, p := range in {
		if err := Validate(p); err != nil {
			errs = append(errs, err)
			continue
		}
		out = append(out, p.Masked())
	}
	return out, errs
}

// SumAddresses — суммарное количество адресов (uint64, IPv6 клампится на MaxUint64).
func SumAddresses(ps []netip.Prefix) uint64 {
	var total uint64
	for _, p := range ps {
		bits := p.Bits()
		hostBits := p.Addr().BitLen() - bits
		if hostBits >= 64 {
			return ^uint64(0)
		}
		n := uint64(1) << hostBits
		if total > ^uint64(0)-n {
			return ^uint64(0)
		}
		total += n
	}
	return total
}
