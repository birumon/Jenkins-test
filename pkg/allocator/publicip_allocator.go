package allocator

import (
	"fmt"
	"net"
	"strings"
	"sync"

	"github.com/mikioh/ipaddr"
	"k8s.io/klog"
)

// PublicIPAllocator allocates public IP from default publicippool for service and floatingip
type PublicIPAllocator struct {
	mutex                sync.RWMutex
	nets                 []*net.IPNet
	usedIPs              map[string]string
	allocatedServices    map[string]string
	allocatedFloatingIPs map[string]string
}

// NewPublicIPAllocator returns new public IP allocator
func NewPublicIPAllocator() *PublicIPAllocator {
	publicIPAllocator := &PublicIPAllocator{
		usedIPs:              make(map[string]string),
		allocatedServices:    make(map[string]string),
		allocatedFloatingIPs: make(map[string]string),
	}

	return publicIPAllocator
}

func parseCIDR(cidr string) ([]*net.IPNet, error) {
	if !strings.Contains(cidr, "-") {
		_, n, err := net.ParseCIDR(cidr)
		if err != nil {
			return nil, fmt.Errorf("Invalid CIDR %q: %s", cidr, err)
		}
		return []*net.IPNet{n}, nil
	}

	fs := strings.SplitN(cidr, "-", 2)
	if len(fs) != 2 {
		return nil, fmt.Errorf("Invalid IP range %q", cidr)
	}
	start := net.ParseIP(strings.TrimSpace(fs[0]))
	if start == nil {
		return nil, fmt.Errorf("Invalid IP range %q: invalid start IP %q", cidr, fs[0])
	}
	end := net.ParseIP(strings.TrimSpace(fs[1]))
	if end == nil {
		return nil, fmt.Errorf("Invalid IP range %q: invalid start IP %q", cidr, fs[1])
	}

	var ret []*net.IPNet
	for _, pfx := range ipaddr.Summarize(start, end) {
		n := &net.IPNet{
			IP:   pfx.IP,
			Mask: pfx.Mask,
		}
		ret = append(ret, n)
	}
	return ret, nil
}

// UpdateIPPool updates IPPool of PublicIPAllocator
// Returns error when target public-addresses are un-updateable.
func (p *PublicIPAllocator) UpdateIPPool(publicAddresses []string) error {
	if len(publicAddresses) == 0 {
		// just delete nets
		klog.Infof("No public-addresses defined, init to nil")

		p.mutex.Lock()
		defer p.mutex.Unlock()

		var emptyNets []*net.IPNet
		p.nets = emptyNets

		// initialize maps
		emptyMap1 := make(map[string]string)
		emptyMap2 := make(map[string]string)
		emptyMap3 := make(map[string]string)

		p.usedIPs = emptyMap1
		p.allocatedFloatingIPs = emptyMap2
		p.allocatedServices = emptyMap3

		return nil
	}

	p.mutex.Lock()
	defer p.mutex.Unlock()

	// Parse input publicAddresses
	var targetNets []*net.IPNet
	for _, cidr := range publicAddresses {
		net, err := parseCIDR(cidr)
		if err != nil {
			return fmt.Errorf("Invalid CIDR %q in public-addresses: %s", cidr, err)
		}
		targetNets = append(targetNets, net...)
	}

	p.nets = targetNets

	// initialize maps
	emptyMap1 := make(map[string]string)
	emptyMap2 := make(map[string]string)
	emptyMap3 := make(map[string]string)

	p.usedIPs = emptyMap1
	p.allocatedFloatingIPs = emptyMap2
	p.allocatedServices = emptyMap3

	return nil
}

// IsUsed returns true if ippool is used
func (p *PublicIPAllocator) IsUsed() bool {
	return len(p.usedIPs) != 0
}

func cidrContainsIP(targetNets []*net.IPNet, targetIP string) bool {
	for _, cidr := range targetNets {
		if cidr.Contains(net.ParseIP(targetIP)) {
			return true
		}
	}

	return false
}

//Determine input ip is in IPPool
func (p *PublicIPAllocator) containsIP(ip net.IP) bool {
	p.mutex.RLock()
	defer p.mutex.RUnlock()

	for _, cidr := range p.nets {
		if cidr.Contains(ip) {
			return true
		}
	}

	return false
}
