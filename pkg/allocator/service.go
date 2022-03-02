package allocator

import (
	"fmt"
	"net"

	"github.com/mikioh/ipaddr"
	"k8s.io/klog"
)

// AllocateService allocates public IP for LoadBalancer type services.
// Returns allocated public IP or error if not allocated.
func (p *PublicIPAllocator) AllocateService(key, desiredIP string) (string, error) {
	var allocatedIP string
	var err error

	if desiredIP != "" {
		allocatedIP, err = p.allocateServiceIP(key, desiredIP)
		if err != nil {
			return "", err
		}
	} else {
		allocatedIP, err = p.allocateServiceIPFromPool(key)
		if err != nil {
			return "", err
		}
	}

	return allocatedIP, nil
}

//allocateServiceIP allocates Specific IP from IP Pool.
func (p *PublicIPAllocator) allocateServiceIP(key, IP string) (string, error) {
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
	if allocatedIP, exists := p.allocatedServices[key]; exists == true {
		if allocatedIP == desiredIP.String() {
			klog.Infof("IP %s already allocated for service %s, no change", desiredIP.String(), key)
			return allocatedIP, nil
		}
		klog.Infof("Service %s has allocated IP %s, but desired IP changed", key, allocatedIP)
	}

	// Try to allocate desired IP.
	// Check if desired IP is in use.
	if _, used := p.usedIPs[desiredIP.String()]; used == true {
		return "", fmt.Errorf("Public IP %s already in use", desiredIP.String())
	}

	p.allocatedServices[key] = desiredIP.String()
	p.usedIPs[desiredIP.String()] = key
	klog.Infof("IP %s allocated from pool (service: %s)", desiredIP.String(), key)

	return desiredIP.String(), nil
}

// allocateServiceIPFromPool allocates unused IP from IP Pool for service.
// Return error when IP Pool is depleted.
func (p *PublicIPAllocator) allocateServiceIPFromPool(key string) (string, error) {
	p.mutex.Lock()
	defer p.mutex.Unlock()

	// If allocated IP for service already exists,
	// then return previously allocated IP.
	if allocatedIP, exists := p.allocatedServices[key]; exists == true {
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
				p.allocatedServices[key] = ip.String()
				p.usedIPs[ip.String()] = key
				klog.Infof("IP %s allocated from pool for Service %s", ip.String(), key)
				return ip.String(), nil
			}
		}
	}

	return "", fmt.Errorf("default-public-ippool depleted")
}

//FreeService frees object and IP from IP Pool
func (p *PublicIPAllocator) FreeService(key string) {
	p.mutex.Lock()
	defer p.mutex.Unlock()

	if _, used := p.allocatedServices[key]; !used {
		klog.Infof("Service %s public IP not exists, but freed", key)
		// no use, but double check
		delete(p.allocatedServices, key)
		return
	}

	ip := p.allocatedServices[key]
	delete(p.usedIPs, ip)
	delete(p.allocatedServices, key)

	klog.Infof("IP %s freed (service: %s)", ip, key)
}
