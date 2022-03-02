package allocator

import (
	"fmt"
	"net"

	"github.com/mikioh/ipaddr"
	"k8s.io/klog"
)

func (p *PublicIPAllocator) AllocateFloatingIP(key, desiredIP string) (string, error) {
	var allocatedIP string
	var err error

	if desiredIP != "" {
		allocatedIP, err = p.allocateFloatingIP(key, desiredIP)
		if err != nil {
			return "", err
		}
	} else {
		allocatedIP, err = p.allocateFloatingIPFromPool(key)
		if err != nil {
			return "", err
		}
	}

	return allocatedIP, nil
}

//allocateFloatingIP allocates Specific IP from IP Pool.
func (p *PublicIPAllocator) allocateFloatingIP(key, IP string) (string, error) {
	// Check if IP is malformed
	desiredIP := net.ParseIP(IP)
	if desiredIP == nil {
		return "", fmt.Errorf("Invalid IP addr format: %s", IP)
	}

	// Check if IP is in IP pool
	if p.containsIP(desiredIP) == false {
		return "", fmt.Errorf("Public IP %s is not in public IP pool", desiredIP.String())
	}

	p.mutex.Lock()
	defer p.mutex.Unlock()

	// Check for allocated IP.
	if allocatedIP, exists := p.allocatedFloatingIPs[key]; exists == true {
		if allocatedIP == desiredIP.String() {
			klog.Infof("IP %s already allocated for floatingip %s, no change", desiredIP.String(), key)
			return allocatedIP, nil
		}
		klog.Infof("FloatingIP %s has allocated IP %s, but desired IP changed", key, allocatedIP)
	}

	// Try to allocate desired IP.
	// Check if desired IP is in use.
	if _, used := p.usedIPs[desiredIP.String()]; used == true {
		return "", fmt.Errorf("Public IP %s already in use", desiredIP.String())
	}

	p.allocatedFloatingIPs[key] = desiredIP.String()
	p.usedIPs[desiredIP.String()] = key
	klog.Infof("IP %s allocated from pool (floatingip: %s)", desiredIP.String(), key)

	return desiredIP.String(), nil
}

// allocateFloatingIPFromPool allocates unused IP from IP Pool for FloatingIP.
// Return error when IP Pool is depleted.
func (p *PublicIPAllocator) allocateFloatingIPFromPool(key string) (string, error) {
	p.mutex.Lock()
	defer p.mutex.Unlock()

	// If allocated IP for floatingIP already exists,
	// then return previously allocated IP.
	if allocatedIP, exists := p.allocatedFloatingIPs[key]; exists == true {
		if key != p.usedIPs[allocatedIP] {
			// If key != p.used[prevIP], then something bug exists...
			klog.Errorf("[FIXME] key != p.used[prevIP]")
		}

		return allocatedIP, nil
	}

	// If not, then allocate new IP and update maps.
	for _, cidr := range p.nets {
		c := ipaddr.NewCursor([]ipaddr.Prefix{*ipaddr.NewPrefix(cidr)})
		for pos := c.First(); pos != nil; pos = c.Next() {
			ip := pos.IP
			if _, used := p.usedIPs[ip.String()]; used == false {
				p.allocatedFloatingIPs[key] = ip.String()
				p.usedIPs[ip.String()] = key
				klog.Infof("IP %s allocated from pool for FloatingIP %s", ip.String(), key)
				return ip.String(), nil
			}
		}
	}

	return "", fmt.Errorf("default-public-ippool depleted")

}

//FreeFloatingIP frees object and IP from IP Pool
func (p *PublicIPAllocator) FreeFloatingIP(key string) {
	p.mutex.Lock()
	defer p.mutex.Unlock()

	if _, used := p.allocatedFloatingIPs[key]; !used {
		klog.Infof("FloatingIP %s public IP not exists, but freed", key)
		// no use, but double check
		delete(p.allocatedFloatingIPs, key)
		return
	}

	ip := p.allocatedFloatingIPs[key]
	delete(p.usedIPs, ip)
	delete(p.allocatedFloatingIPs, key)

	klog.Infof("IP %s freed (floatingip: %s)", ip, key)
}
