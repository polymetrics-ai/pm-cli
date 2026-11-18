//go:build linux

package main

import (
	"context"
	"net"
	"os"
	"strings"
	"syscall"
	"testing"
)

// linuxIPPROTOMPTCP is IPPROTO_MPTCP of <linux/in.h>, for the test only.
const linuxIPPROTOMPTCP = 0x106

// Go developer test, not acceptance proof (ADR 0008, rule 3; #405). Defect:
// on Linux the check refuses an inherited listener at the configured address
// because its protocol is IPPROTO_MPTCP and not IPPROTO_TCP. Go makes an
// MPTCP listener for "tcp" when the kernel has MPTCP turned on, and so does
// the bind of pm-server without the flag. Each case builds its listener on
// purpose with net.ListenConfig and SetMultipathTCP, so the case does not
// depend on the default of Go. The plain TCP case runs on each Linux kernel.
// A kernel with MPTCP turned off, or with no MPTCP, gives a plain TCP socket
// for the MPTCP request. The MPTCP case then skips and gives the reason.
func TestInheritedListenerProtocols(t *testing.T) {
	for _, test := range []struct {
		name     string
		multi    bool
		protocol int
	}{
		{"plain TCP", false, syscall.IPPROTO_TCP},
		{"MPTCP", true, linuxIPPROTOMPTCP},
	} {
		t.Run(test.name, func(t *testing.T) {
			var config net.ListenConfig
			config.SetMultipathTCP(test.multi)
			listener, err := config.Listen(context.Background(), "tcp4", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			file, err := listener.(*net.TCPListener).File()
			_ = listener.Close()
			if err != nil {
				t.Fatal(err)
			}
			defer file.Close()
			protocol, err := syscall.GetsockoptInt(int(file.Fd()), syscall.SOL_SOCKET, syscall.SO_PROTOCOL)
			if err != nil {
				t.Fatal(err)
			}
			if protocol != test.protocol {
				if test.multi && protocol == syscall.IPPROTO_TCP {
					t.Skipf("the kernel gave IPPROTO_TCP for an MPTCP listener: MPTCP is turned off or absent on this kernel (net.mptcp.enabled: %s)", mptcpEnabled())
				}
				t.Fatalf("test setup: the listener has protocol %d, want %d", protocol, test.protocol)
			}
			descriptor := inheritedCopy(t, int(file.Fd()))
			listen := strings.TrimPrefix(socketName(descriptor), "inet4 ")
			inherited, err := inheritListenDescriptor(descriptor, listen)
			if err != nil {
				_ = syscall.Close(descriptor)
				t.Fatalf("the check refused an inherited %s listener at the configured address: %v", test.name, err)
			}
			defer inherited.Close()
			if got := inherited.Addr().String(); got != listen {
				t.Errorf("listener address %q, want the configured address %q", got, listen)
			}
		})
	}
}

// mptcpEnabled gives the value of the net.mptcp.enabled sysctl, or the
// reason that the test cannot read it.
func mptcpEnabled() string {
	value, err := os.ReadFile("/proc/sys/net/mptcp/enabled")
	if err != nil {
		return "not readable: " + err.Error()
	}
	return strings.TrimSpace(string(value))
}
