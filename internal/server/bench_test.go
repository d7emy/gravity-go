package server

import "testing"

// isLocalHost sits on the CORS path of every request carrying an Origin header.
// Resolving the machine's own addresses per call meant a ~2ms interface-
// enumeration syscall on every LAN-origin request; they are cached once instead.
// This guards against that regressing: a cached lookup is sub-microsecond, a
// syscall is emphatically not.
func BenchmarkIsLocalHostLANAddress(b *testing.B) {
	for i := 0; i < b.N; i++ {
		isLocalHost("192.168.1.50")
	}
}

func BenchmarkIsLocalHostLoopback(b *testing.B) {
	for i := 0; i < b.N; i++ {
		isLocalHost("localhost")
	}
}
