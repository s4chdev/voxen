package daemon

import (
	"context"
	"fmt"
	"net"
	"strings"

	"github.com/s4chdev/voxen/internal/ipc"
)

// networkReport describes this machine's connectivity for diagnostics.
//
// The question it answers is the one that decides whether a cross-network call will
// work: which interfaces can carry media, and is a private overlay available? Without
// it a user only discovers symmetric NAT when a call fails.
type networkReport struct {
	interfaces []interfaceInfo
	// overlay is the name of a detected private overlay (Tailscale, WireGuard), or
	// empty. Its presence is the most useful signal: a call over an overlay behaves
	// like a LAN call regardless of NAT.
	overlay string
}

type interfaceInfo struct {
	name  string
	addrs []string
	kind  string // "loopback", "overlay", "private", "public"
}

func inspectNetwork() networkReport {
	var rep networkReport
	ifaces, err := net.Interfaces()
	if err != nil {
		return rep
	}
	for _, iface := range ifaces {
		if iface.Flags&net.FlagUp == 0 {
			continue
		}
		addrs, err := iface.Addrs()
		if err != nil {
			continue
		}
		info := interfaceInfo{name: iface.Name}
		kind := "public"
		if iface.Flags&net.FlagLoopback != 0 {
			kind = "loopback"
		} else if isOverlayInterface(iface.Name) {
			kind = "overlay"
			if rep.overlay == "" {
				rep.overlay = iface.Name
			}
		}
		for _, a := range addrs {
			ipnet, ok := a.(*net.IPNet)
			if !ok || ipnet.IP.To4() == nil {
				continue
			}
			info.addrs = append(info.addrs, ipnet.IP.String())
			if kind == "public" && ipnet.IP.IsPrivate() {
				kind = "private"
			}
		}
		if len(info.addrs) == 0 {
			continue
		}
		info.kind = kind
		rep.interfaces = append(rep.interfaces, info)
	}
	return rep
}

// isOverlayInterface recognises the common private-overlay interface names.
func isOverlayInterface(name string) bool {
	lower := strings.ToLower(name)
	for _, prefix := range []string{"tailscale", "wg", "zt", "utun", "nebula", "wireguard"} {
		if strings.HasPrefix(lower, prefix) {
			return true
		}
	}
	return false
}

// networkCheck reports connectivity and, when a private overlay is present and not
// configured, points out that using it avoids NAT entirely.
func (s *Server) networkCheck(_ context.Context) []ipc.DoctorCheck {
	rep := inspectNetwork()

	var parts []string
	for _, iface := range rep.interfaces {
		if iface.kind == "loopback" {
			continue
		}
		parts = append(parts, fmt.Sprintf("%s (%s, %s)", iface.name, iface.kind, strings.Join(iface.addrs, ", ")))
	}
	summary := "no usable network interfaces found"
	if len(parts) > 0 {
		summary = strings.Join(parts, "; ")
	}

	var checks []ipc.DoctorCheck
	checks = append(checks, ipc.DoctorCheck{
		Name:    "network-interfaces",
		Status:  "pass",
		Message: summary,
	})

	filter := s.cfg.Network.InterfaceFilter
	switch {
	case len(filter) > 0:
		checks = append(checks, ipc.DoctorCheck{
			Name:   "interface-filter",
			Status: "pass",
			Message: fmt.Sprintf("ICE restricted to %s; every candidate will be on that interface",
				strings.Join(filter, ", ")),
		})
	case rep.overlay != "":
		// The useful advice: an overlay is present and would remove the NAT question
		// entirely, but gathering is still using every interface.
		checks = append(checks, ipc.DoctorCheck{
			Name:    "interface-filter",
			Status:  "warn",
			Message: fmt.Sprintf("%s is available but ICE is gathering on every interface", rep.overlay),
			Hint: fmt.Sprintf("for reliable calls across networks, set network.interface_filter: [%s] and restart; "+
				"candidates then stay on the overlay and NAT never matters", rep.overlay),
		})
	default:
		checks = append(checks, ipc.DoctorCheck{
			Name:    "interface-filter",
			Status:  "pass",
			Message: "gathering on every interface; direct calls depend on NAT traversal",
			Hint: "if calls fail between different networks, a private overlay (Tailscale, WireGuard) " +
				"plus network.interface_filter makes them behave like a LAN call",
		})
	}

	return checks
}
